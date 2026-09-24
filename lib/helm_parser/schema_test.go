package helm_parser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/chart/common"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
)

func chartWithSchema(schema string) *chartv2.Chart {
	return &chartv2.Chart{
		Metadata:  &chartv2.Metadata{APIVersion: chartv2.APIVersionV2, Name: "schema-chart", Version: "1.0.0"},
		Values:    map[string]any{"image": "nginx:1"},
		Schema:    []byte(schema),
		Templates: []*common.File{deploymentTemplate("{{ .Values.image }}")},
	}
}

func TestImageSchemaCannotReadServerFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-schema.json")
	const secret = "LOCAL_SCHEMA_SECRET"
	if err := os.WriteFile(path, []byte(`{"properties":{"image":{"const":"`+secret+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	chart := chartWithSchema(`{"$ref":"file://` + path + `"}`)
	_, err := GetChartImages(t.Context(), chart, nil, false)
	if err == nil {
		t.Fatal("server-local schema reference was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("schema validation error exposed local file contents: %v", err)
	}
}

func TestImageSchemaCannotFetchExternalResources(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"type":"object"}`))
	}))
	defer server.Close()

	for _, tc := range []struct {
		name     string
		subchart bool
		bundled  bool
		cancel   bool
		expired  bool
	}{
		{name: "root schema"},
		{name: "subchart schema", subchart: true},
		{name: "bundled schema", bundled: true},
		{name: "canceled request", cancel: true},
		{name: "expired deadline", expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chart := chartWithSchema(`{"$ref":"` + server.URL + `/schema.json"}`)
			if tc.bundled {
				chart.Schema = []byte(`{"$ref":"schemas/inner.json"}`)
				chart.Files = []*common.File{{Name: "schemas/inner.json", Data: []byte(`{"$ref":"` + server.URL + `/schema.json"}`)}}
			}
			if tc.subchart {
				parent := chartWithSchema(`{"type":"object"}`)
				parent.AddDependency(chart)
				chart = parent
			}
			ctx := t.Context()
			if tc.cancel || tc.expired {
				var cancel context.CancelFunc
				if tc.cancel {
					ctx, cancel = context.WithCancel(ctx)
				} else {
					ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				}
				cancel()
			}
			_, err := GetChartImages(ctx, chart, nil, true)
			if err == nil {
				t.Fatal("external schema reference was accepted")
			}
			if tc.cancel && !errors.Is(err, context.Canceled) {
				t.Errorf("canceled schema validation returned %v, want context.Canceled", err)
			}
			if tc.expired && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("expired schema validation returned %v, want context.DeadlineExceeded", err)
			}
			if got := hits.Load(); got != 0 {
				t.Errorf("external schema server received %d requests", got)
			}
		})
	}
}

func TestImageSchemaUsesSubchartBundle(t *testing.T) {
	parent := chartWithSchema(`{"type":"object"}`)
	subchart := chartWithSchema(`{"properties":{"image":{"$ref":"schemas/image.json#/$defs/name"}}}`)
	subchart.Metadata.Name = "child"
	subchart.Files = []*common.File{{Name: "schemas/image.json", Data: []byte(`{"$defs":{"name":{"const":"nginx:1"}}}`)}}
	parent.AddDependency(subchart)

	if _, err := GetChartImages(t.Context(), parent, nil, true); err != nil {
		t.Fatalf("bundled subchart schema was rejected: %v", err)
	}
	if _, err := GetChartImages(t.Context(), parent, map[string]any{"child": map[string]any{"image": "redis:1"}}, true); err == nil {
		t.Error("bundled subchart schema did not validate child values")
	}
}

func TestImageSchemaValidatesInDocumentAndBundledReferences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		bundle bool
	}{
		{name: "in-document", schema: `{"properties":{"image":{"$ref":"#/$defs/name"}},"$defs":{"name":{"type":"string","pattern":"^nginx:"}}}`},
		{name: "bundled", bundle: true, schema: `{"properties":{"image":{"$ref":"schemas/image.json#/$defs/name"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chart := chartWithSchema(tc.schema)
			if tc.bundle {
				chart.Files = []*common.File{{Name: "schemas/image.json", Data: []byte(`{"$defs":{"name":{"type":"string","pattern":"^nginx:"}}}`)}}
			}
			images, err := GetChartImages(t.Context(), chart, nil, false)
			if err != nil || len(images) != 1 || images[0].FullImage != "nginx:1" {
				t.Fatalf("valid chart images = %v, error = %v", images, err)
			}
			if _, err := GetChartImages(t.Context(), chart, map[string]any{"image": "redis:1"}, false); err == nil {
				t.Error("schema did not reject invalid image values")
			}
		})
	}
}
