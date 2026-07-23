package daemon

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

//go:embed interaction.schema.json
var interactionSchema []byte

func TestWireBuildMatchesCanonicalSchema(t *testing.T) {
	decoder := json.NewDecoder(bytes.NewReader(interactionSchema))
	decoder.UseNumber()
	var schema any
	if err := decoder.Decode(&schema); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	want := "com.yasyf.cc-interact.interaction/" + hex.EncodeToString(sum[:]) + "/v1"
	if WireBuild != want {
		t.Fatalf("WireBuild = %q, want %q", WireBuild, want)
	}
	if !strings.HasSuffix(WireBuild, "/v1") {
		t.Fatalf("WireBuild = %q, want exact v1 namespace", WireBuild)
	}
}

func TestCanonicalSchemaMatchesProtocolStructs(t *testing.T) {
	var schema struct {
		Definitions map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(interactionSchema, &schema); err != nil {
		t.Fatal(err)
	}
	tests := map[string]reflect.Type{
		"Envelope":      reflect.TypeFor[Envelope](),
		"Reply":         reflect.TypeFor[Reply](),
		"RuntimeHealth": reflect.TypeFor[RuntimeHealth](),
	}
	for name, protocolType := range tests {
		t.Run(name, func(t *testing.T) {
			properties, ok := schema.Definitions[name]
			if !ok {
				t.Fatalf("schema is missing %q", name)
			}
			want := slices.Sorted(maps.Keys(properties.Properties))
			var got []string
			for index := range protocolType.NumField() {
				tag := strings.Split(protocolType.Field(index).Tag.Get("json"), ",")[0]
				if tag != "" && tag != "-" {
					got = append(got, tag)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("Go JSON fields = %v, schema fields = %v", got, want)
			}
		})
	}
}

func TestWireBuildHardCutRejectsAnotherSchema(t *testing.T) {
	if _, err := NewClient(t.Context(), ClientConfig{WireBuild: "legacy"}); err == nil {
		t.Fatal("NewClient accepted another wire schema")
	}
	if _, err := New(Config{WireBuild: "legacy"}); err == nil {
		t.Fatal("New accepted another wire schema")
	}
	if _, err := (Launcher{WireBuild: "legacy"}).NewClient(t.Context()); err == nil {
		t.Fatal("Launcher accepted another wire schema")
	}
}
