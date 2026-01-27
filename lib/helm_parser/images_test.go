package helm_parser

import (
	"testing"
)

func TestParseImageString(t *testing.T) {
	tests := []struct {
		name     string
		image    string
		expected ImageReference
	}{
		{
			name:  "simple image name",
			image: "nginx",
			expected: ImageReference{
				Registry:   "docker.io",
				Repository: "library/nginx",
				Tag:        "latest",
				FullImage:  "nginx",
			},
		},
		{
			name:  "image with tag",
			image: "nginx:1.25",
			expected: ImageReference{
				Registry:   "docker.io",
				Repository: "library/nginx",
				Tag:        "1.25",
				FullImage:  "nginx:1.25",
			},
		},
		{
			name:  "docker hub user repo",
			image: "bitnami/nginx:1.25.0",
			expected: ImageReference{
				Registry:   "docker.io",
				Repository: "bitnami/nginx",
				Tag:        "1.25.0",
				FullImage:  "bitnami/nginx:1.25.0",
			},
		},
		{
			name:  "fully qualified docker hub",
			image: "docker.io/library/nginx:1.25",
			expected: ImageReference{
				Registry:   "docker.io",
				Repository: "library/nginx",
				Tag:        "1.25",
				FullImage:  "docker.io/library/nginx:1.25",
			},
		},
		{
			name:  "gcr.io image",
			image: "gcr.io/project/image:v1",
			expected: ImageReference{
				Registry:   "gcr.io",
				Repository: "project/image",
				Tag:        "v1",
				FullImage:  "gcr.io/project/image:v1",
			},
		},
		{
			name:  "ghcr.io image",
			image: "ghcr.io/owner/repo/image:latest",
			expected: ImageReference{
				Registry:   "ghcr.io",
				Repository: "owner/repo/image",
				Tag:        "latest",
				FullImage:  "ghcr.io/owner/repo/image:latest",
			},
		},
		{
			name:  "registry with port",
			image: "registry.example.com:5000/app:v1",
			expected: ImageReference{
				Registry:   "registry.example.com:5000",
				Repository: "app",
				Tag:        "v1",
				FullImage:  "registry.example.com:5000/app:v1",
			},
		},
		{
			name:  "image with digest",
			image: "nginx@sha256:abc123def456",
			expected: ImageReference{
				Registry:   "docker.io",
				Repository: "library/nginx",
				Tag:        "",
				Digest:     "sha256:abc123def456",
				FullImage:  "nginx@sha256:abc123def456",
			},
		},
		{
			name:  "fully qualified with digest",
			image: "gcr.io/project/image@sha256:abc123",
			expected: ImageReference{
				Registry:   "gcr.io",
				Repository: "project/image",
				Tag:        "",
				Digest:     "sha256:abc123",
				FullImage:  "gcr.io/project/image@sha256:abc123",
			},
		},
		{
			name:  "image with both tag and digest",
			image: "nginx:1.25@sha256:abc123def456",
			expected: ImageReference{
				Registry:   "docker.io",
				Repository: "library/nginx",
				Tag:        "1.25",
				Digest:     "sha256:abc123def456",
				FullImage:  "nginx:1.25@sha256:abc123def456",
			},
		},
		{
			name:  "fully qualified with tag and digest",
			image: "gcr.io/project/image:v2.0@sha256:abc123",
			expected: ImageReference{
				Registry:   "gcr.io",
				Repository: "project/image",
				Tag:        "v2.0",
				Digest:     "sha256:abc123",
				FullImage:  "gcr.io/project/image:v2.0@sha256:abc123",
			},
		},
		{
			name:  "quay.io image",
			image: "quay.io/prometheus/prometheus:v2.45.0",
			expected: ImageReference{
				Registry:   "quay.io",
				Repository: "prometheus/prometheus",
				Tag:        "v2.45.0",
				FullImage:  "quay.io/prometheus/prometheus:v2.45.0",
			},
		},
		{
			name:  "empty string",
			image: "",
			expected: ImageReference{
				Registry:   "",
				Repository: "",
				Tag:        "latest",
				FullImage:  "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseImage(tt.image)

			if result.Registry != tt.expected.Registry {
				t.Errorf("Registry: got %q, want %q", result.Registry, tt.expected.Registry)
			}
			if result.Repository != tt.expected.Repository {
				t.Errorf("Repository: got %q, want %q", result.Repository, tt.expected.Repository)
			}
			if result.Tag != tt.expected.Tag {
				t.Errorf("Tag: got %q, want %q", result.Tag, tt.expected.Tag)
			}
			if result.Digest != tt.expected.Digest {
				t.Errorf("Digest: got %q, want %q", result.Digest, tt.expected.Digest)
			}
			if result.FullImage != tt.expected.FullImage {
				t.Errorf("FullImage: got %q, want %q", result.FullImage, tt.expected.FullImage)
			}
		})
	}
}

func TestExtractImagesFromManifests(t *testing.T) {
	tests := []struct {
		name      string
		manifests []string
		expected  int // number of expected images
	}{
		{
			name: "deployment with single container",
			manifests: []string{
				`apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
spec:
  template:
    spec:
      containers:
      - name: nginx
        image: nginx:1.25`,
			},
			expected: 1,
		},
		{
			name: "deployment with init container",
			manifests: []string{
				`apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  template:
    spec:
      initContainers:
      - name: init
        image: busybox:1.35
      containers:
      - name: app
        image: myapp:v1`,
			},
			expected: 2,
		},
		{
			name: "cronjob",
			manifests: []string{
				`apiVersion: batch/v1
kind: CronJob
metadata:
  name: backup
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: backup
            image: backup-tool:latest`,
			},
			expected: 1,
		},
		{
			name: "pod",
			manifests: []string{
				`apiVersion: v1
kind: Pod
metadata:
  name: test-pod
spec:
  containers:
  - name: main
    image: alpine:3.18`,
			},
			expected: 1,
		},
		{
			name: "multiple documents",
			manifests: []string{
				`apiVersion: apps/v1
kind: Deployment
metadata:
  name: frontend
spec:
  template:
    spec:
      containers:
      - name: frontend
        image: frontend:v1
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: backend
spec:
  template:
    spec:
      containers:
      - name: backend
        image: backend:v2`,
			},
			expected: 2,
		},
		{
			name: "statefulset",
			manifests: []string{
				`apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: database
spec:
  template:
    spec:
      containers:
      - name: db
        image: postgres:15`,
			},
			expected: 1,
		},
		{
			name: "daemonset",
			manifests: []string{
				`apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: monitoring
spec:
  template:
    spec:
      containers:
      - name: agent
        image: monitoring-agent:v1`,
			},
			expected: 1,
		},
		{
			name:      "empty manifest",
			manifests: []string{""},
			expected:  0,
		},
		{
			name: "non-workload resource",
			manifests: []string{
				`apiVersion: v1
kind: ConfigMap
metadata:
  name: config
data:
  key: value`,
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractImagesFromManifests(tt.manifests)
			if len(result) != tt.expected {
				t.Errorf("got %d images, want %d", len(result), tt.expected)
				for i, img := range result {
					t.Logf("  image[%d]: %+v", i, img)
				}
			}
		})
	}
}

func TestDeduplicateImages(t *testing.T) {
	images := []ImageReference{
		{FullImage: "nginx:1.25", Source: "Deployment/frontend"},
		{FullImage: "nginx:1.25", Source: "Deployment/backend"},
		{FullImage: "redis:7", Source: "Deployment/cache"},
	}

	result := deduplicateImages(images)

	if len(result) != 2 {
		t.Errorf("got %d images, want 2", len(result))
	}

	// Find the nginx image and check sources are combined
	for _, img := range result {
		if img.FullImage == "nginx:1.25" {
			if img.Source != "Deployment/frontend, Deployment/backend" && img.Source != "Deployment/backend, Deployment/frontend" {
				t.Errorf("sources not combined properly: %q", img.Source)
			}
		}
	}
}

func TestExtractFromPodSpec(t *testing.T) {
	obj := map[string]interface{}{
		"spec": map[interface{}]interface{}{
			"containers": []interface{}{
				map[interface{}]interface{}{
					"name":  "main",
					"image": "myapp:v1",
				},
			},
			"initContainers": []interface{}{
				map[interface{}]interface{}{
					"name":  "init",
					"image": "busybox:1.35",
				},
			},
		},
	}

	images := extractFromPodSpec(obj, []string{"spec"}, "Pod/test")

	if len(images) != 2 {
		t.Errorf("got %d images, want 2", len(images))
	}

	hasMain := false
	hasInit := false
	for _, img := range images {
		if img.FullImage == "myapp:v1" && img.Source == "Pod/test" {
			hasMain = true
		}
		if img.FullImage == "busybox:1.35" && img.Source == "Pod/test (init)" {
			hasInit = true
		}
	}

	if !hasMain {
		t.Error("main container image not found")
	}
	if !hasInit {
		t.Error("init container image not found")
	}
}
