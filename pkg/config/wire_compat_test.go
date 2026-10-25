package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Characterization tests for the two config types whose serialized shape is a
// stored-data contract rather than an internal detail.
//
// SecureString is the reason config.json and .security.yml are not
// interchangeable. The mechanism is a Go-specific trick — IsZero() inspects
// runtime.Caller(2) and reports "empty" unless the caller lives in yaml.v* —
// and its observable effect is narrower than it looks. Measured behaviour:
//
//   - JSON: MarshalJSON always returns the literal sentinel "[NOT_HERE]".
//     omitempty does NOT suppress it: a non-nil pointer and a non-empty slice
//     are both emitted as the sentinel, and a VALUE-typed SecureString is
//     emitted even when it is the zero value. So config.json on disk contains
//     "[NOT_HERE]" wherever a secret was set.
//   - YAML: MarshalYAML returns the real value (or the enc:// / file://
//     reference unchanged), and omitempty works, so unset secrets vanish.
//
// A port must reproduce the sentinel exactly — user configs already contain it
// — and must keep the "decode is a no-op" rule, because that is what makes
// config.json non-authoritative for secrets.

type compatSecretHolder struct {
	Plain   string        `json:"plain,omitempty"  yaml:"plain,omitempty"`
	Secret  *SecureString `json:"secret,omitempty" yaml:"secret,omitempty"`
	NoOmit  *SecureString `json:"no_omit"          yaml:"no_omit"`
	Multi   SecureStrings `json:"multi,omitempty"  yaml:"multi,omitempty"`
	MultiNo SecureStrings `json:"multi_no"         yaml:"multi_no"`
}

// value-typed holder: omitempty cannot suppress a SecureString value.
type compatSecretValueHolder struct {
	Zero SecureString `json:"zero,omitempty" yaml:"zero,omitempty"`
	Set  SecureString `json:"set,omitempty"  yaml:"set,omitempty"`
}

const notHereSentinel = `"[NOT_HERE]"`

func TestCompat_SecureStringJSONEmitsSentinelYAMLEmitsValue(t *testing.T) {
	in := compatSecretHolder{
		Plain:   "visible",
		Secret:  NewSecureString("hunter2"),
		NoOmit:  NewSecureString("hunter2"),
		Multi:   SimpleSecureStrings("a", "b"),
		MultiNo: SimpleSecureStrings("a", "b"),
	}

	jsonOut, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	wantJSON := `{"plain":"visible","secret":` + notHereSentinel +
		`,"no_omit":` + notHereSentinel +
		`,"multi":` + notHereSentinel +
		`,"multi_no":` + notHereSentinel + `}`
	if string(jsonOut) != wantJSON {
		t.Errorf("JSON shape differs\n got: %s\nwant: %s", jsonOut, wantJSON)
	}

	yamlOut, err := yaml.Marshal(in)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	for _, want := range []string{"hunter2", "- a", "- b"} {
		if !strings.Contains(string(yamlOut), want) {
			t.Errorf("YAML must carry the real secret %q, got:\n%s", want, yamlOut)
		}
	}
	if strings.Contains(string(yamlOut), "[NOT_HERE]") {
		t.Errorf("YAML must never carry the sentinel, got:\n%s", yamlOut)
	}
}

// TestCompat_SecureStringOmitEmptyIsIneffectiveInJSON is the sharpest
// asymmetry: an ENTIRELY EMPTY value-typed SecureString still serializes as the
// sentinel in JSON, while YAML omits it. A port that models "secret" as
// Option<Secret> with skip_serializing_if will silently change which keys
// appear in every config.json it writes.
func TestCompat_SecureStringOmitEmptyIsIneffectiveInJSON(t *testing.T) {
	var empty compatSecretValueHolder
	empty.Set = *NewSecureString("value")

	jsonOut, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if want := `{"zero":` + notHereSentinel + `,"set":` + notHereSentinel + `}`; string(jsonOut) != want {
		t.Errorf("JSON\n got: %s\nwant: %s", jsonOut, want)
	}

	yamlOut, err := yaml.Marshal(empty)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if strings.Contains(string(yamlOut), "zero") {
		t.Errorf("YAML must omit the unset secret, got:\n%s", yamlOut)
	}
	if !strings.Contains(string(yamlOut), "value") {
		t.Errorf("YAML must include the set secret, got:\n%s", yamlOut)
	}
}

// TestCompat_JSONRoundTripDestroysSecrets documents the consequence of the
// sentinel: writing a config as JSON and reading it back does not preserve a
// secret that was set in memory. This is intentional (config.json is not the
// secret store — .security.yml is), but it is silent, so a port must fail in the
// same way rather than "helpfully" persisting the plaintext.
func TestCompat_JSONRoundTripDestroysSecrets(t *testing.T) {
	original := compatSecretHolder{Secret: NewSecureString("hunter2"), Multi: SimpleSecureStrings("k1")}

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), "[NOT_HERE]") {
		t.Fatalf("expected the sentinel in %s", raw)
	}

	var back compatSecretHolder
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Secret != nil && back.Secret.String() != "" {
		t.Errorf("the sentinel must decode to no value, got %q", back.Secret.String())
	}
	if len(back.Multi) != 0 {
		t.Errorf("the sentinel slice must decode to empty, got %d entries", len(back.Multi))
	}
}

func TestCompat_SecureStringDecodesRealValues(t *testing.T) {
	// file:// is resolved EAGERLY, at decode time, and is relative to the
	// config directory with containment enforced AFTER EvalSymlinks. An
	// absolute path outside that directory is rejected outright. A port that
	// resolves lazily, or that accepts absolute paths, changes both when the
	// error surfaces and what a user is allowed to write.
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "key"), []byte("  from-file\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "empty"), nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	updateResolver(configDir)
	t.Cleanup(func() { updateResolver("") })

	tests := []struct {
		name string
		json string
		want string
	}{
		{"plaintext", `{"secret":"hunter2"}`, "hunter2"},
		{"empty string", `{"secret":""}`, ""},
		{"file reference is resolved and trimmed at decode time", `{"secret":"file://key"}`, "from-file"},
		{"sentinel decodes to a no-op", `{"secret":` + notHereSentinel + `}`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h compatSecretHolder
			if err := json.Unmarshal([]byte(tt.json), &h); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			got := h.Secret.String()
			if got != tt.want {
				t.Errorf("SecureString = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCompat_SecureStringFileReferenceContainment pins the two rejection modes:
// a path outside the config directory, and a missing file.
func TestCompat_SecureStringFileReferenceContainment(t *testing.T) {
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "empty"), nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	updateResolver(configDir)
	t.Cleanup(func() { updateResolver("") })

	for _, ref := range []string{
		`{"secret":"file:///etc/passwd"}`,
		`{"secret":"file://../outside"}`,
		`{"secret":"file://missing"}`,
		// An empty file is rejected too: only the literal empty STRING means
		// "no credential". A port that treats a blank file as a blank key will
		// boot where picoclaw refuses to.
		`{"secret":"file://empty"}`,
	} {
		var h compatSecretHolder
		if err := json.Unmarshal([]byte(ref), &h); err == nil {
			t.Errorf("%s must fail the decode, got %q", ref, h.Secret.String())
		}
	}
}

// TestCompat_SecureStringsDeduplicates pins that SimpleSecureStrings removes
// duplicates while preserving first-seen order — channel allow-lists are built
// through it, so a port that keeps duplicates changes what is written to disk.
func TestCompat_SecureStringsDeduplicates(t *testing.T) {
	got := SimpleSecureStrings("b", "a", "b", "c", "a")
	want := []string{"b", "a", "c"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got.Values())
	}
	for i, w := range want {
		if got[i].String() != w {
			t.Errorf("index %d = %q, want %q", i, got[i].String(), w)
		}
	}
	if vals := (*SecureStrings)(&got).Values(); len(vals) != 3 {
		t.Errorf("Values() = %v, want 3 entries", vals)
	}
}

func TestCompat_SecureStringYAMLPreservesReferences(t *testing.T) {
	// A value that is already a reference must be written back as the reference,
	// never encrypted a second time.
	for _, ref := range []string{"enc://gAAAA", "file:///run/secrets/key"} {
		out, err := yaml.Marshal(NewSecureString(ref))
		if err != nil {
			t.Fatalf("yaml.Marshal(%q): %v", ref, err)
		}
		if strings.TrimSpace(string(out)) != ref {
			t.Errorf("yaml.Marshal(%q) = %q, want the reference unchanged", ref, strings.TrimSpace(string(out)))
		}
	}
}

func TestCompat_FlexibleStringSliceJSONForms(t *testing.T) {
	tests := []struct {
		name string
		json string
		want []string
	}{
		{"null becomes nil", `null`, nil},
		{"bare string is wrapped", `"alice"`, []string{"alice"}},
		{"bare number is formatted without decimals", `12345`, []string{"12345"}},
		{"large number keeps no exponent", `1234567890123`, []string{"1234567890123"}},
		{"fractional number is rounded by %.0f", `2.5`, []string{"2"}},
		{"string array", `["a","b"]`, []string{"a", "b"}},
		{"empty array", `[]`, []string{}},
		{"mixed array stringifies every element", `["a",1,true,null]`, []string{"a", "1", "true", "<nil>"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got FlexibleStringSlice
			if err := json.Unmarshal([]byte(tt.json), &got); err != nil {
				t.Fatalf("Unmarshal(%s): %v", tt.json, err)
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("null must produce nil, got %#v", []string(got))
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Unmarshal(%s) = %v, want %v", tt.json, []string(got), tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("Unmarshal(%s)[%d] = %q, want %q", tt.json, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestCompat_FlexibleStringSliceRejectsObjects bounds the flexibility: the type
// accepts string, number, array of scalars — but NOT an object. A port that
// reaches for a permissive "anything" deserializer will accept configs picoclaw
// rejects, which changes what a user can get away with writing.
func TestCompat_FlexibleStringSliceRejectsObjects(t *testing.T) {
	var got FlexibleStringSlice
	if err := json.Unmarshal([]byte(`{"k":"v"}`), &got); err == nil {
		t.Fatalf("an object must be rejected, got %#v", []string(got))
	}
}

// TestCompat_FlexibleStringSliceEnvForms pins the env-var path, which is a
// DIFFERENT parser from the JSON one: it splits on ASCII and full-width commas
// and drops empty fields. A port that reuses one parser for both paths will
// stop honouring PICOCLAW_*_ALLOW_FROM="1，2".
func TestCompat_FlexibleStringSliceEnvForms(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
		// nilWant distinguishes "nil" from "empty non-nil", which the two
		// parsers do differently: JSON [] yields an empty non-nil slice, env
		// ",," yields an empty non-nil slice, env "" yields nil.
		nilWant bool
	}{
		{"empty text is nil", "", nil, true},
		{"single value", "alice", []string{"alice"}, false},
		{"ascii comma", "a,b", []string{"a", "b"}, false},
		{"full-width comma", "a，b", []string{"a", "b"}, false},
		{"mixed commas", "a，b,c", []string{"a", "b", "c"}, false},
		{"surrounding spaces trimmed", " a , b ", []string{"a", "b"}, false},
		{"empty fields dropped", "a,,b,", []string{"a", "b"}, false},
		{"all empty fields is empty but not nil", ",,", []string{}, false},
		{"internal spaces kept", "a b,c", []string{"a b", "c"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got FlexibleStringSlice
			if err := got.UnmarshalText([]byte(tt.text)); err != nil {
				t.Fatalf("UnmarshalText(%q): %v", tt.text, err)
			}
			if tt.nilWant {
				if got != nil {
					t.Fatalf("want nil, got %#v", []string(got))
				}
				return
			}
			if got == nil {
				t.Fatalf("UnmarshalText(%q) = nil, want %v", tt.text, tt.want)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("UnmarshalText(%q) = %v, want %v", tt.text, []string(got), tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("UnmarshalText(%q)[%d] = %q, want %q", tt.text, i, got[i], tt.want[i])
				}
			}
		})
	}
}
