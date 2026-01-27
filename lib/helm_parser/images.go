package helm_parser

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v2"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/engine"
)

type ImageReference struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest,omitempty"`
	FullImage  string `json:"fullImage"`
	Source     string `json:"source"`
}

func parseImage(image string) ImageReference {
	ref := ImageReference{
		FullImage: image,
		Tag:       "latest",
	}

	if image == "" {
		return ref
	}

	workingImage := image

	// image@sha256:... or image:tag@sha256:...
	if idx := strings.LastIndex(workingImage, "@"); idx != -1 {
		ref.Digest = workingImage[idx+1:]
		workingImage = workingImage[:idx]
		ref.Tag = "" // Default to empty when digest is used
	}

	// image:tag
	if idx := strings.LastIndex(workingImage, ":"); idx != -1 {
		afterColon := workingImage[idx+1:]
		if !strings.Contains(afterColon, "/") {
			ref.Tag = afterColon
			workingImage = workingImage[:idx]
		}
	}

	parts := strings.Split(workingImage, "/")

	switch len(parts) {
	case 1:
		// nginx => docker.io/library/nginx
		ref.Registry = "docker.io"
		ref.Repository = "library/" + parts[0]
	case 2:
		// docker.io/library/nginx => registry: docker.io, repository: library/nginx
		if strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") {
			ref.Registry = parts[0]
			ref.Repository = parts[1]
		} else {
			// library/nginx => docker.io/library/nginx
			ref.Registry = "docker.io"
			ref.Repository = workingImage
		}
	default:
		// registry/repo/image => registry: registry, repository: repo/image
		ref.Registry = parts[0]
		ref.Repository = strings.Join(parts[1:], "/")
	}

	return ref
}

func GetChartImages(chart *chartv2.Chart, customValues map[string]interface{}, recursive bool) ([]ImageReference, error) {
	manifests, err := renderChart(chart, customValues)
	if err != nil {
		return nil, err
	}

	images := extractImagesFromManifests(manifests)

	if recursive {
		for _, subChart := range chart.Dependencies() {
			subImages, err := GetChartImages(subChart, customValues, recursive)
			if err != nil {
				return nil, fmt.Errorf("failed to render subchart %s: %v", subChart.Name(), err)
			}
			images = append(images, subImages...)
		}
	}

	images = deduplicateImages(images)
	sort.Slice(images, func(i, j int) bool {
		return images[i].FullImage < images[j].FullImage
	})

	return images, nil
}

func renderChart(chart *chartv2.Chart, customValues map[string]interface{}) ([]string, error) {
	options := common.ReleaseOptions{
		Name:      "release-name",
		Namespace: "default",
		Revision:  1,
		IsUpgrade: false,
		IsInstall: true,
	}

	caps := common.DefaultCapabilities
	valuesToRender, err := util.ToRenderValues(chart, customValues, options, caps)
	if err != nil {
		return nil, err
	}

	e := engine.Engine{Strict: false, LintMode: true}
	rendered, err := e.Render(chart, valuesToRender)
	if err != nil {
		return nil, err
	}

	manifests := make([]string, 0, len(rendered))
	for _, content := range rendered {
		if strings.TrimSpace(content) != "" {
			manifests = append(manifests, content)
		}
	}

	return manifests, nil
}

func extractImagesFromManifests(manifests []string) []ImageReference {
	var images []ImageReference

	for _, manifest := range manifests {
		docs := strings.Split(manifest, "---")
		for _, doc := range docs {
			doc = strings.TrimSpace(doc)
			if doc == "" {
				continue
			}

			extracted := extractImagesFromDocument(doc)
			images = append(images, extracted...)
		}
	}

	return images
}

func extractImagesFromDocument(doc string) []ImageReference {
	var obj map[string]interface{}
	if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
		return nil
	}

	kind, _ := obj["kind"].(string)
	metadata, _ := obj["metadata"].(map[interface{}]interface{})
	name := ""
	if metadata != nil {
		name, _ = metadata["name"].(string)
	}
	source := kind
	if name != "" {
		source = kind + "/" + name
	}

	var images []ImageReference

	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet":
		images = extractFromPodSpec(obj, []string{"spec", "template", "spec"}, source)
	case "Job":
		images = extractFromPodSpec(obj, []string{"spec", "template", "spec"}, source)
	case "CronJob":
		images = extractFromPodSpec(obj, []string{"spec", "jobTemplate", "spec", "template", "spec"}, source)
	case "Pod":
		images = extractFromPodSpec(obj, []string{"spec"}, source)
	}

	return images
}

func extractFromPodSpec(obj map[string]interface{}, path []string, source string) []ImageReference {
	spec := navigateToPath(obj, path)
	if spec == nil {
		return nil
	}

	var images []ImageReference

	if containers, ok := spec["containers"].([]interface{}); ok {
		for _, c := range containers {
			if container, ok := c.(map[interface{}]interface{}); ok {
				if image, ok := container["image"].(string); ok && image != "" {
					ref := parseImage(image)
					ref.Source = source
					images = append(images, ref)
				}
			}
		}
	}

	if initContainers, ok := spec["initContainers"].([]interface{}); ok {
		for _, c := range initContainers {
			if container, ok := c.(map[interface{}]interface{}); ok {
				if image, ok := container["image"].(string); ok && image != "" {
					ref := parseImage(image)
					ref.Source = source + " (init)"
					images = append(images, ref)
				}
			}
		}
	}

	return images
}

func navigateToPath(obj map[string]interface{}, path []string) map[interface{}]interface{} {
	var current interface{} = obj

	for _, key := range path {
		switch v := current.(type) {
		case map[string]interface{}:
			current = v[key]
		case map[interface{}]interface{}:
			current = v[key]
		default:
			return nil
		}
	}

	if result, ok := current.(map[interface{}]interface{}); ok {
		return result
	}
	return nil
}

func deduplicateImages(images []ImageReference) []ImageReference {
	seen := make(map[string]ImageReference)

	for _, img := range images {
		if existing, ok := seen[img.FullImage]; ok {
			if !strings.Contains(existing.Source, img.Source) {
				existing.Source = existing.Source + ", " + img.Source
				seen[img.FullImage] = existing
			}
		} else {
			seen[img.FullImage] = img
		}
	}

	result := make([]ImageReference, 0, len(seen))
	for _, img := range seen {
		result = append(result, img)
	}

	return result
}
