package helm_parser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"helm.sh/helm/v4/pkg/chart/common"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
)

const chartSchemaURL = "file:///chart/values.schema.json"

// chartSchemaLoader resolves only files carried inside the current chart. The
// compiler must never fall back to its default file or network loaders.
type chartSchemaLoader map[string][]byte

func (l chartSchemaLoader) Load(rawURL string) (any, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "file" || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/chart/") {
		return nil, errors.New("schema resource is not bundled with this chart")
	}
	data, ok := l[strings.TrimPrefix(u.Path, "/chart/")]
	if !ok {
		return nil, errors.New("schema resource is not bundled with this chart")
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(data))
}

func validateChartSchema(chart *chartv2.Chart, values map[string]any) (result error) {
	defer func() {
		if recover() != nil {
			result = errors.New("unable to validate chart schema")
		}
	}()

	schema, err := jsonschema.UnmarshalJSON(bytes.NewReader(chart.Schema))
	if err != nil {
		return err
	}
	files := make(chartSchemaLoader, len(chart.Files))
	for _, file := range chart.Files {
		files[file.Name] = file.Data
	}

	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(files)
	if err := compiler.AddResource(chartSchemaURL, schema); err != nil {
		return err
	}
	validator, err := compiler.Compile(chartSchemaURL)
	if err != nil {
		return err
	}
	if err := validator.Validate(values); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "jsonschema validation failed with '"+chartSchemaURL+"#'\n") + "\n")
	}
	return nil
}

// validateChartSchemas follows Helm's subchart validation rules, but compiles
// schemas without the filesystem, network, or global URN resolver.
func validateChartSchemas(ctx context.Context, chart *chartv2.Chart, values common.Values) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var failures strings.Builder
	if chart.Schema != nil {
		if err := validateChartSchema(chart, values.AsMap()); err != nil {
			fmt.Fprintf(&failures, "%s:\n%s", chart.Name(), err)
		}
	}
	for _, subchart := range chart.Dependencies() {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, exists := values[subchart.Name()]
		if !exists || raw == nil {
			continue
		}
		subValues, ok := raw.(map[string]any)
		if !ok {
			fmt.Fprintf(&failures, "%s:\ninvalid type for values: expected object (map), got %T\n", subchart.Name(), raw)
			continue
		}
		if err := validateChartSchemas(ctx, subchart, common.Values(subValues)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures.WriteString(err.Error())
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if failures.Len() > 0 {
		return errors.New(failures.String())
	}
	return nil
}
