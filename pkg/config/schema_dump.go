//go:build ignore

// Extracts the JSON field map that pkg/config/diagnostics.go builds by
// reflection, so the Rust port can refuse the same keys without a reflect
// package.
//
// Run with a package directory and a root struct, which is what the port needs today:
//
//	go run -tags goolm,stdjson pkg/config/schema_dump.go pkg/config Config > schema.json
//
// The tags are the ones the package builds with; without them the goolm build variant
// changes which files parse and the dump silently describes a different package.
//
// It is `//go:build ignore` on purpose: it is a maintenance tool, not part of the
// package or the test suite, and it must not be compiled into a release binary.
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// node is one schema entry: the JSON names a document may use at this position,
// and what each of them leads to.
type node struct {
	Kind string `json:"kind"` // struct | map | slice | scalar
	// Fields is set for kind=struct: json name -> child.
	Fields map[string]*node `json:"fields,omitempty"`
	// Elem is set for kind=map and kind=slice.
	Elem *node `json:"elem,omitempty"`
	// Name is the Go type, for the error message and for a human reading the dump.
	Name string `json:"name,omitempty"`
	// Omit is the field's encoding option: "omitzero", "omitempty", or absent.
	// The validator does not need it -- an unknown key is unknown either way -- but
	// the *writer* does: a field carrying omitzero disappears from the document when
	// its value is zero, which is why a config with an empty session has no session
	// key at all. Without this the port has to guess, and a wrong guess writes a key
	// Go leaves out.
	Omit string `json:"omit,omitempty"`
	// YAML is set for kind=struct: the fields yaml.v3 would emit for this struct, in
	// declaration order, which is the order they appear in .security.yml.
	//
	// It is a list and Fields is a map on purpose. The validator only ever asks "is
	// this key known here", which a map answers; the security writer has to reproduce
	// a *byte sequence*, and a map marshals sorted. Config's declaration order is
	// channel_list, model_list, then the inlined Tools' own web and skills -- which is
	// not sorted order (web and skills would land after session, isolation, …), so a
	// port driven off the map writes a file with the right content in the wrong order.
	//
	// A field carrying yaml:"-" is absent from this list entirely. That is how the
	// whole file stays small: Config marks every non-secret field yaml:"-", which is
	// why .security.yml has no `version` key even though yaml.v3 emits untagged fields
	// under their lowercased Go names.
	YAML []yamlField `json:"yaml_fields,omitempty"`
	// JSONFields is set for kind=struct: the keys encoding/json emits for this struct,
	// in declaration order, with each one's omit option.
	//
	// It exists for the same reason YAML does, one encoder over: a map marshals sorted,
	// so a port that walks Fields writes the right keys in the wrong order. It matters
	// most inside a channel's `settings`, because InitChannelList decodes the stored
	// JSON into the typed struct for the channel's type and the loaded value then
	// marshals *that* -- TelegramSettings declares base_url and proxy before
	// use_markdown_v2, so the loaded config emits them first, and a sorted port emits
	// them last. The list is also what says which fields a settings object has at all:
	// a field with no omit option is emitted even when its value is the zero value, so
	// `dingtalk` carries `"client_id": ""` and `slack` carries an empty object rather
	// than no `settings` key. Neither is visible in a dump of the stored document,
	// because neither is in it.
	//
	// An embedded struct splices its own list in at that position. That models the
	// no-conflict half of typeFields: a promoted field *also* loses to a shallower one
	// with the same name, and same-depth ties are dropped outright -- the rule that
	// hoists `session` above `version` at the root (see the Config case in structNode).
	// No colliding key exists in the tree today, so the splice is exact as things stand;
	// it stops being exact the day someone adds one. A field tagged json:"-" is absent,
	// as from Fields.
	JSONFields []jsonField `json:"json_fields,omitempty"`
	// BaseFields is set on the Channel node only: the JSON keys that belong to Channel
	// itself rather than to a channel's settings. pkg/config/migration.go decides which
	// half of a legacy flat channel entry moves inside `settings` by asking this exact
	// Go map, so the port has to have the same list -- and a list copied into Rust is a
	// list that silently disagrees the day someone adds a channel-level field.
	BaseFields []string `json:"base_fields,omitempty"`
	// SettingsSchemas is set on the Channel node only: channel type -> the struct its
	// settings decode into, with that struct's yaml and json emission lists.
	SettingsSchemas map[string]*node `json:"settings_schemas,omitempty"`
	// SingletonChannelTypes is set on the Channel node only: the singleton registry,
	// read off the package's own `registerSingletonChannel` calls and resolved through
	// the channel-type constants. validateSingletonChannels counts enabled channels per
	// type against exactly this set, and the registry is built at init() time by calls
	// scattered through the package -- there is no literal to copy, which is why the port
	// cannot know it without the dump.
	SingletonChannelTypes []string `json:"singleton_channel_types,omitempty"`
	// StreamingValidated marks a settings struct that validateChannelStreamingConfig
	// names in its type switch, so its `streaming` bounds are checked. It is a flag on
	// the settings node rather than a list of channel types because the switch is written
	// over structs: the function's default arm returns nil, so a channel whose settings
	// struct is not named there loads any bounds the file carries. Measured, and the
	// agreement with "has a streaming field" is exact today -- but the switch is the
	// source, and a struct that grows a streaming field without joining the switch is a
	// change the flag follows and a derived rule would not.
	StreamingValidated bool `json:"streaming_validated,omitempty"`
	// JSONMarshaler says this type declares its own `MarshalJSON()`, so the bytes that
	// go out are whatever that method returns and `json_fields` is at best the shape it
	// usually builds from. Read it as "do not write this node from json_fields alone".
	//
	// It is deliberately a flag rather than a suppression of `json_fields`, because
	// Channel declares one and its list *is* exactly what goes out -- the type-alias
	// trick inside MarshalJSON marshals the declaration order. Suppressing on the flag
	// would throw away the one list the port most needs.
	//
	// The flag fires on 49 nodes covering nine types in a real Config. Eight of the
	// nine lie about their bytes in one of three ways; Channel is the honest one. All
	// nine are named, with the bytes measured off the real types, on ctx.jsonMarshalers
	// -- an unlisted name is a claim that the shape is honest, so the list is exhaustive
	// on purpose. The absence of `json_fields` on a flagged node is itself a claim:
	// SecureString is a struct with no exported fields, so it has no list, but its
	// MarshalJSON writes the *string* "[NOT_HERE]" -- 36 positions in a real config.
	JSONMarshaler bool `json:"json_marshaler,omitempty"`
	// Marshaler says this type implements yaml.Marshaler, so its own MarshalYAML
	// decides what is emitted and the generic walk below must not run for it. Four
	// types in pkg/config do: SecureModelList, Channel, SkillRegistryConfig and
	// SkillsRegistriesConfig -- each of which emits a hand-built "only the secrets"
	// projection rather than the struct's fields.
	Marshaler bool `json:"marshaler,omitempty"`
	// EnvIndex is set on the root node only: every environment variable env.Parse
	// binds, and the document position it writes. Flat, not a tree -- see envBinding.
	//
	// pkg/config carries 200 `env` tags over 195 distinct names plus 26 `envPrefix`
	// tags, and no other kind of option: measured, zero tags in the package carry a
	// comma, so there is no `,omitempty`, `,default`, `,required` or `,expandVar` to
	// model. A port cannot copy that list by hand. The name a variable answers to is
	// not the string at the field that carries it -- `envPrefix` on an ancestor is
	// prepended at parse time -- and one type (`ToolConfig`) is embedded under 23
	// different prefixes, so the same field answers to PICOCLAW_TOOLS_WEB_ENABLED
	// and PICOCLAW_TOOLS_EXEC_ENABLED depending on the path taken to it. Deriving the
	// table here is the same defence as the rest of the dump: a port once dropped
	// `web` while walking into `brave` because it decided from the root schema, and a
	// hand-copied env list is that bug with a longer literal.
	//
	// What the table decides is what the corpus grades: whether a load refuses at all
	// (env.Parse aborts the whole load on a mistyped value while the compat shims
	// ignore one silently -- so strictness is per variable, not global) and what the
	// loaded value and saved bytes then say. The refusal text is not graded: a failing
	// case records only `error_op_index`, and Go's message names the Go struct field
	// rather than a JSON path, so no consumer needs byte parity with it. `go` is
	// carried for exactly that reason -- it is what the message names.
	EnvIndex []envBinding `json:"env_index,omitempty"`
	// ShimVars is set on the root node only: the variables the hand-written compat
	// shims read. They are listed apart from EnvIndex because a shim can write a
	// position no tag points at (PICOCLAW_HOME lands in a path built by
	// filepath.Join, not at a tagged field), and a strict tag and a shim can name the
	// same variable (then the strict layer runs first, because LoadConfig calls
	// env.Parse before the shims).
	ShimVars []envShim `json:"shim_vars,omitempty"`
	// EnvPasses states, once per pass, what happens to a value the parser cannot use.
	// It is a sibling of the rows rather than a column because the rule belongs to the
	// pass, and a row disagreeing with it can only mean this dump is wrong.
	EnvPasses []envPass `json:"env_passes,omitempty"`
}

// envPass is one env.Parse call site and its failure policy.
type envPass struct {
	// Name matches envBinding.Pass.
	Name string `json:"name"`
	// Site is the Go function and file that calls env.Parse, so a row's provenance is
	// greppable the same way a shim reader is.
	Site string `json:"site"`
	// BadValue is what a value the parser rejects does: "refuse_load" aborts the load
	// and names the Go field; "ignore" leaves the field at whatever the file said.
	BadValue string `json:"bad_value"`
	// EntryRequired is true when the binding exists only for an entry of a matching
	// channel_list key: no entry, no binding.
	EntryRequired bool `json:"entry_required,omitempty"`
}

// envBinding is one row of the root's env index: a variable and the document
// position env.Parse writes for it.
//
// It is per (variable, position) rather than per variable, because the library
// walks fields, not names: one name bound at two positions sets both, and a port
// that de-duplicated by name would write one. Duplicates are counted in the
// commit that adds this rather than assumed away.
type envBinding struct {
	// Var is the fully resolved name: every ancestor's envPrefix, in the order the
	// walk meets them, prepended to the tag's own name. This is the string the
	// operating system carries, so a consumer never composes names.
	Var string `json:"var"`
	// Path is the JSON key sequence from the root document to the field, using the
	// same names the validator refuses unknown keys by. An embedded struct adds no
	// segment, because encoding/json inlines it.
	Path []string `json:"path"`
	// Go is the Go field name: what env.Parse's error message names, and the only
	// stable identifier for a field whose JSON name is shared with cousins.
	Go string `json:"go"`
	// Type is the type the parser targets, which is what makes a value mistyped:
	// int refuses "abc", bool refuses "yes", string refuses nothing. A named type
	// reports `Name:underlying` so a consumer that knows FlexibleStringSlice or
	// SecureStrings can dispatch on the name and still see the shape underneath.
	Type string `json:"type"`
	// Indexed marks a position reached through a slice of structs. The library parses
	// element i under the prefix `<p><i>_` and builds only as many elements as it
	// finds variables for, so the real name has an index spliced in and is unbounded
	// at dump time. The path spells the index `0` as the pattern; a consumer matches
	// the `<prefix><digits>_` shape rather than a literal name.
	Indexed bool `json:"indexed,omitempty"`
	// Pass is which of the two env.Parse calls binds this field: "root" for
	// env.Parse(cfg) in LoadConfig, "channel" for the per-entry env.Parse(target) in
	// InitChannelList (config_channel.go:750). They disagree on the one thing a port
	// cannot guess: an unparseable value at a root field aborts the whole load, while
	// the same at a channel field is swallowed by that pass's own Non-fatal comment.
	// Measured both ways with the probe at picoclaw 6ab4ab27: a bad int at
	// agents.defaults.context_window refuses the load naming the field, while a bad
	// bool at a channel settings field leaves the field at its file value and the load
	// still succeeds.
	Pass string `json:"pass"`
	// Secret is set when the field's type masks its own value: SecureString and
	// SecureStrings. Measured with the probe at picoclaw 6ab4ab27 -- a document that
	// carries tools/web/brave/api_keys and tools/web/baidu_search/api_key loads fine,
	// and neither key is present in the loaded projection or in the saved file; a
	// variable naming the same field changes neither. So every row flagged here has no
	// observable through the marshaller -- the count is not written down because it is
	// a property of the tag set, not of this file: they are emitted because the binding
	// is real (env.Parse writes the field, and runtime behaviour depends on it) and
	// flagged because a case that sets one cannot be graded by comparing bytes. Grading them
	// needs the op kind that reports a field through an accessor, which is the same
	// piece #34 deferred for the marshal-stage-collapse question.
	Secret bool `json:"secret,omitempty"`
	// ParseNote marks the one row whose parse target does not behave like its type.
	ParseNote string `json:"parse_note,omitempty"`
	// PrefixNote marks a row whose name is the enclosing envPrefix joined to a tag
	// that already begins with that same prefix. The duplication is not a generator
	// artefact: the loader joins prefix and tag unconditionally, so the doubled name
	// is the one it reads and the shorter one is dead. Do not "correct" the name here.
	PrefixNote string `json:"prefix_note,omitempty"`
	// ChannelTypes is set for pass "channel": the channel types whose settings struct
	// carries this field, from channelSettingsFactory. The position is not a static
	// path -- it hangs off a channel_list entry whose `type` names one of these -- so
	// a consumer applies the row per matching entry rather than once per document.
	// An entry of that type has to exist: the defaults carry one entry for 16 types and
	// none for mqtt, vk or pico_client, so a variable for those three binds nothing
	// until the file creates such an entry.
	ChannelTypes []string `json:"channel_types,omitempty"`
}

// envShim is one compat-shim variable: the name a shim looks up, the function that
// looks it up, and whether a strict tag binds the same name.
//
// These are the non-strict half of the layer. Measured 2026-11-14 (one process per
// shape at picoclaw `9dbecf98`): `..._CLAWHUB_ENABLED=yes` left `enabled: true` in
// place, `..._CLAWHUB_TIMEOUT=abc` added no key at all, and
// PICOCLAW_CHANNELS_TELEGRAM_STREAMING_THROTTLE_SECONDS=x changed nothing -- while
// a bad int at a tagged field aborted the load and left both input files
// byte-identical. A port that treats every variable as strict refuses files Go
// loads; one that treats every variable as lenient loads files Go refuses.
//
// The reader name is emitted because these are code, not tags: the row's provenance
// is a call site, and a reviewer checking whether the list is current greps for it.
type envShim struct {
	Var    string `json:"var"`
	Reader string `json:"reader"`
	Tagged bool   `json:"tagged,omitempty"`
}

// yamlField is one entry of a struct's yaml emission list.
type yamlField struct {
	// JSON is the key this field holds in the JSON document, which is the document
	// the writer walks. It is not always the yaml key: the two agree today, but a
	// field tagged `json:"a" yaml:"b"` would make them differ, and a writer that
	// assumed they were the same would read the right value and label it wrong.
	JSON string `json:"json"`
	// Name is the key yaml.v3 writes: the yaml tag's name, or the lowercased Go field
	// name when the field carries no yaml tag at all.
	Name string `json:"name"`
	// Untagged marks the second case, so the port can tell "tagged with this name"
	// from "no tag, so yaml.v3 lowercased it".
	Untagged bool `json:"untagged,omitempty"`
	// Inline is `yaml:",inline"`: the field's own children are emitted at this level
	// and the field itself never appears. Tools carries it, which is the only reason
	// web and skills are top-level sections of .security.yml.
	Inline bool `json:"inline,omitempty"`
	// Omit is the yaml option: "omitempty" or absent. yaml.v3 has no omitzero.
	Omit string `json:"omit,omitempty"`
}

// jsonField is one entry of a struct's JSON emission list. The child node -- kind,
// element type, nested fields -- is in Fields under the same key; this list carries
// only what a map cannot: the order, and whether the key survives a zero value.
type jsonField struct {
	// JSON is the key encoding/json writes for this field.
	JSON string `json:"json"`
	// Omit is "omitzero", "omitempty", or absent. Absent means the key is written even
	// when the value is zero, which is the case most of a channel's settings fields are.
	Omit string `json:"omit,omitempty"`
}

// omitOption reads the encoding option out of a json tag: the part after the name.
func omitOption(tag *ast.BasicLit) string {
	if tag == nil {
		return ""
	}
	v, err := strconv.Unquote(tag.Value)
	if err != nil {
		return ""
	}
	parts := strings.Split(reflect.StructTag(v).Get("json"), ",")
	if len(parts) < 2 {
		return ""
	}
	switch parts[1] {
	case "omitzero", "omitempty":
		return parts[1]
	}
	return ""
}

type ctx struct {
	fset    *token.FileSet
	structs map[string]*ast.StructType
	// aliases is every other named type in the directory -- `type ChannelsConfig
	// map[string]*Channel` and friends. Without it a named map or slice looks like
	// a scalar, and the validator would then accept any key inside one: exactly the
	// class of hole this dump exists to close.
	aliases map[string]ast.Expr
	named   map[string]bool
	// baseFields is `var BaseFieldNames` from config_channel.go, read off the literal.
	baseFields []string
	// settingsTypes is `var channelSettingsFactory`: channel type -> the name of the
	// Settings struct its settings block decodes into. InitChannelList decodes every
	// channel into that struct and Channel.MarshalYAML emits it, so the .security.yml
	// writer cannot decide what a channel contributes without it.
	settingsTypes map[string]string
	// consts resolves the factory's keys, which are named constants (ChannelTelegram)
	// rather than string literals.
	consts map[string]string
	// singletonRefs is every `registerSingletonChannel` argument in the package, kept as
	// the parsed expression because the constants it names may be declared in a file the
	// loop has not reached yet. Resolved once the whole directory has been read.
	singletonRefs []ast.Expr
	// singletonTypes is singletonRefs resolved and sorted: the registry
	// validateSingletonChannels counts against.
	singletonTypes []string
	// streamingChecked is the type switch inside validateChannelStreamingConfig: the
	// settings structs whose streaming bounds are validated. Everything else takes the
	// function's default arm.
	streamingChecked map[string]bool
	// cache stops a recursive type from being walked twice, and makes a cycle
	// terminate: a self-referential struct gets a scalar placeholder the second
	// time, which is what reflect would report as the same shape anyway.
	cache map[string]*node
	// files keeps every parsed file so the compat-shim scan can run after the whole
	// directory is read. It has to: the shims look their names up through constants
	// (envSkillsClawHubEnabled, EnvGatewayHost) declared in another file, so a call
	// site met mid-loop would resolve to nothing -- the same reason singletonRefs are
	// held as expressions and resolved once every constant is known.
	files []*ast.File
	// fieldReachable is every struct name the root walk touched. It is what lets the
	// channel pass refuse a settings struct that is *also* reachable through the
	// document: two passes binding one field is a claim this dump cannot make.
	fieldReachable map[string]bool
	// yamlMarshalers is every type in the directory that declares its own
	// `MarshalYAML()`. The security writer has to know, because for those types the
	// emitted node is whatever that method returns -- not the struct's fields -- and
	// running the generic walk instead produces a file with the struct's shape where
	// Go writes a hand-built secrets-only projection.
	yamlMarshalers map[string]bool
	// jsonMarshalers is every type in the directory that declares its own
	// `MarshalJSON()`. Nine do, covering 49 nodes of a real Config, and for eight of
	// them the bytes that go out are not the struct's fields. Measured, by what the
	// lie is:
	//
	// A struct that writes a scalar. SecureString (36 nodes) and SecureStrings (5:
	// model_list[].api_keys and the four web-search api_keys) both write the *string*
	// "[NOT_HERE]". SecureStrings is a []*SecureString, so a porter reading only the
	// kind writes `["[NOT_HERE]"]` where Go writes `"[NOT_HERE]"` -- and it writes that
	// for a populated slice, an empty one and a nil one alike, so no shape distinction
	// survives the round trip.
	//
	// A struct that writes a bare string: AgentModelConfig (2) emits `"m"` for
	// {Primary:"m"}, not an object.
	//
	// A slice that writes an object. SkillsRegistriesConfig (1: tools.skills.registries)
	// is []*SkillRegistryConfig and writes a name-keyed object with the keys sorted --
	// {zeta, alpha} marshals to {"alpha":{...},"zeta":{...}} -- `null` when nil but `{}`
	// when empty and non-nil, and entries that are nil or carry an empty Name are
	// dropped ({keep, "", nil} emits one member). The name is the key and never a
	// member. Its element SkillRegistryConfig (1) writes a hand-built map too: `enabled`
	// and `base_url` always, `auth_token` only when the token is non-empty, plus every
	// Param key that is not empty, not "param", not "_"-prefixed and does not collide --
	// so its json_fields list names `name`, which never appears, and cannot name the
	// user keys that do.
	//
	// A scalar that writes stored bytes. RawNode (1: channel_list[].settings) emits the
	// bytes verbatim -- {"z":1,"a":2} keeps its order, because nothing re-encodes it --
	// and `null` when empty or nil.
	//
	// A struct that shadows itself. EvolutionConfig (1) lists 9 keys and emits at most
	// 7; Config (1) wraps an aux struct that hoists `session` above every promoted
	// field, which is why a present `session` is the first key in the file.
	//
	// The ninth, Channel (1), declares one whose output *is* the declaration order --
	// which is why this is a flag and not a suppression.
	jsonMarshalers map[string]bool
}

// tagYAML reads a struct tag's yaml part.
//
// Returns the emission key, whether the field is inlined, its omit option, and
// whether the field is hidden. `yaml:"-"` hides; no yaml key at all means yaml.v3
// lowercases the Go field name, which is a third state and is reported as untagged
// rather than folded into either of the other two.
func tagYAML(tag *ast.BasicLit, goName string) (name string, inline bool, omit string, hidden, untagged bool) {
	if tag == nil {
		return strings.ToLower(goName), false, "", false, true
	}
	v, err := strconv.Unquote(tag.Value)
	if err != nil {
		return strings.ToLower(goName), false, "", false, true
	}
	y, ok := reflect.StructTag(v).Lookup("yaml")
	if !ok {
		return strings.ToLower(goName), false, "", false, true
	}
	parts := strings.Split(y, ",")
	if parts[0] == "-" {
		return "", false, "", true, false
	}
	for _, opt := range parts[1:] {
		switch opt {
		case "inline":
			inline = true
		case "omitempty":
			omit = "omitempty"
		}
	}
	name = parts[0]
	if name == "" {
		name = strings.ToLower(goName)
	}
	return name, inline, omit, false, false
}

func main() {
	dir := os.Args[1]
	fset := token.NewFileSet()
	c := &ctx{
		fset:             fset,
		structs:          map[string]*ast.StructType{},
		aliases:          map[string]ast.Expr{},
		named:            map[string]bool{},
		consts:           map[string]string{},
		streamingChecked: map[string]bool{},
		settingsTypes:    map[string]string{},
		cache:            map[string]*node{},
		yamlMarshalers:   map[string]bool{},
		jsonMarshalers:   map[string]bool{},
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, entry := range files {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, dir+"/"+entry.Name(), nil, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		c.files = append(c.files, f)
		// Two pieces of loader behaviour live in code rather than in a literal, and both
		// are things the port has to know exactly:
		//
		//   - the singleton registry. Each channel type calls registerSingletonChannel
		//     from an init(), so there is no map to read off; the call sites are the
		//     registry. Their arguments are kept unresolved (see singletonRefs) because
		//     the constant may be declared in a file this loop has not reached.
		//   - the streaming validator's type switch. validateChannelStreamingConfig
		//     checks the bounds only for the structs its switch names and returns nil
		//     for every other settings struct, so "which channels are checked" is a fact
		//     about that function, not about which structs happen to declare a field.
		ast.Inspect(f, func(n ast.Node) bool {
			switch nd := n.(type) {
			case *ast.CallExpr:
				if fn, ok := nd.Fun.(*ast.Ident); ok && fn.Name == "registerSingletonChannel" && len(nd.Args) == 1 {
					c.singletonRefs = append(c.singletonRefs, nd.Args[0])
				}
			case *ast.FuncDecl:
				if nd.Name.Name != "validateChannelStreamingConfig" || nd.Body == nil {
					return true
				}
				for _, stmt := range nd.Body.List {
					sw, ok := stmt.(*ast.TypeSwitchStmt)
					if !ok {
						continue
					}
					for _, clause := range sw.Body.List {
						cc, ok := clause.(*ast.CaseClause)
						if !ok {
							continue
						}
						for _, t := range cc.List {
							if name := strings.TrimPrefix(typeString(t), "*"); name != "" {
								c.streamingChecked[name] = true
							}
						}
					}
				}
			}
			return true
		})
		for _, decl := range f.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, n := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						if bl, ok := vs.Values[i].(*ast.BasicLit); ok && bl.Kind == token.STRING {
							if v, err := strconv.Unquote(bl.Value); err == nil {
								c.consts[n.Name] = v
							}
						}
					}
				}
			}
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.VAR {
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) == 0 || vs.Names[0].Name != "BaseFieldNames" {
						continue
					}
					collectKeys(vs.Values[0], &c.baseFields)
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) == 0 || vs.Names[0].Name != "channelSettingsFactory" {
						continue
					}
					lit, ok := vs.Values[0].(*ast.CompositeLit)
					if !ok {
						continue
					}
					for _, elt := range lit.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						c.settingsTypes[constName(kv.Key, c.consts)] = structOf(kv.Value)
					}
				}
			}
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil {
				switch fn.Name.Name {
				case "MarshalYAML":
					c.yamlMarshalers[receiverName(fn.Recv)] = true
					continue
				case "MarshalJSON":
					c.jsonMarshalers[receiverName(fn.Recv)] = true
					continue
				}
			}
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					c.aliases[ts.Name.Name] = ts.Type
					continue
				}
				c.structs[ts.Name.Name] = st
				c.named[ts.Name.Name] = true
			}
		}
	}

	// The singleton registry, resolved now that every constant in the package is known.
	// Sorted because the dump is embedded and compared byte for byte: the call order is
	// an artifact of which file the init() sits in.
	seenSingleton := map[string]bool{}
	for _, arg := range c.singletonRefs {
		name := constName(arg, c.consts)
		if name == "" {
			// A registration this cannot resolve would silently shrink the registry, and
			// the port would then accept two enabled channels of a type Go refuses.
			fmt.Fprintf(os.Stderr, "registerSingletonChannel: cannot resolve argument %s\n", typeString(arg))
			os.Exit(1)
		}
		if !seenSingleton[name] {
			seenSingleton[name] = true
			c.singletonTypes = append(c.singletonTypes, name)
		}
	}
	sort.Strings(c.singletonTypes)

	root, ok := c.structs[os.Args[2]]
	if !ok {
		fmt.Fprintf(os.Stderr, "no struct %s in %s\n", os.Args[2], dir)
		os.Exit(1)
	}
	out := c.structNode(os.Args[2], root, 0)
	// The env index is built from the root, after the tree exists, because a
	// variable's name is only complete once every ancestor prefix on the path to it
	// is known. It deliberately does not live on the nodes a field is reached
	// through: structNode caches one node per named type and `ToolConfig` is reached
	// under six prefixes, so a resolved name stored on a cached node would report
	// whichever path the walk happened to take first. One traversal from the root has
	// no such state.
	var bindings []envBinding
	c.collectEnv(os.Args[2], "", nil, 0, map[string]bool{}, &bindings)
	sort.SliceStable(bindings, func(i, j int) bool {
		if bindings[i].Var != bindings[j].Var {
			return bindings[i].Var < bindings[j].Var
		}
		return strings.Join(bindings[i].Path, ".") < strings.Join(bindings[j].Path, ".")
	})
	tags := map[string]bool{}
	for _, b := range bindings {
		tags[b.Var] = true
	}
	// A shim name no strict tag carries is normal (PICOCLAW_HOME, PICOCLAW_LOG_LEVEL);
	// a strict name a shim also reads is the interesting case, and `tagged` says so.
	// The channel pass. InitChannelList decodes every channel_list entry into its
	// per-type settings struct and calls env.Parse on that struct, so those tags bind
	// too -- just not through the root's type graph, which is why the walk above finds
	// none of them (Channel.Settings is a RawNode). A port that replays only the root
	// pass silently drops 79 of the package's 192 bindable env sites.
	bySettings := map[string][]string{}
	for chanType, settingsName := range c.settingsTypes {
		if settingsName == "" {
			continue
		}
		bySettings[settingsName] = append(bySettings[settingsName], chanType)
	}
	for _, settingsName := range sortedKeys(bySettings) {
		types := bySettings[settingsName]
		sort.Strings(types)
		if c.fieldReachable[settingsName] {
			// A struct reached by both passes would bind twice, from the document and
			// from the entry, and the dump would say one thing while Go did another.
			fmt.Fprintf(os.Stderr, "%s is reachable from the root AND from a channel entry: refusing to emit two bindings for one field\n", settingsName)
			os.Exit(1)
		}
		var rows []envBinding
		c.collectEnv(settingsName, "", nil, 0, map[string]bool{}, &rows)
		for _, row := range rows {
			row.Pass = "channel"
			row.ChannelTypes = types
			bindings = append(bindings, row)
		}
	}
	sort.SliceStable(bindings, func(i, j int) bool {
		if bindings[i].Var != bindings[j].Var {
			return bindings[i].Var < bindings[j].Var
		}
		if bindings[i].Pass != bindings[j].Pass {
			return bindings[i].Pass < bindings[j].Pass
		}
		return strings.Join(bindings[i].Path, ".") < strings.Join(bindings[j].Path, ".")
	})
	for i := range bindings {
		if bindings[i].Pass == "" {
			bindings[i].Pass = "root"
		}
		tags[bindings[i].Var] = true
	}
	out.EnvPasses = []envPass{
		{Name: "root", Site: "LoadConfig (config.go:1518)", BadValue: "refuse_load"},
		{Name: "channel", Site: "InitChannelList (config_channel.go:750)", BadValue: "ignore", EntryRequired: true},
	}
	out.EnvIndex = bindings
	out.ShimVars = c.collectShims(tags)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	_ = enc.Encode(out)
}

// structNode walks one struct the way populateJSONFieldTypeMap does: unexported
// fields skipped, `json:"-"` skipped, an anonymous field with no name inlined, an
// untagged field named by its Go name.
func (c *ctx) structNode(name string, st *ast.StructType, depth int) *node {
	if cached, ok := c.cache[name]; ok && depth > 0 {
		return cached
	}
	out := &node{Kind: "struct", Fields: map[string]*node{}, Name: name,
		Marshaler: c.yamlMarshalers[name], JSONMarshaler: c.jsonMarshalers[name]}
	if name == "Channel" {
		out.BaseFields = c.baseFields
		out.SingletonChannelTypes = c.singletonTypes
		// Each channel type's settings struct, walked with its own yaml table. This is
		// the only place the security writer learns that `telegram` answers to
		// TelegramSettings -- the field's own type is RawNode, so nothing in the type
		// graph says it.
		out.SettingsSchemas = map[string]*node{}
		for chanType, settingsName := range c.settingsTypes {
			if settingsName == "" || !c.named[settingsName] {
				continue
			}
			out.SettingsSchemas[chanType] = c.nodeFor(ast.NewIdent(settingsName), depth)
		}
	}
	if c.streamingChecked[name] {
		// Reached through SettingsSchemas, so the flag lands on the settings node a
		// channel of that type decodes into.
		out.StreamingValidated = true
	}
	if depth > 0 {
		c.cache[name] = out
	}
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			// embedded
			tn := typeString(field.Type)
			// Only a named struct declared in this package can be walked. That excludes
			// an embedded *pointer* (`typeString` yields "*ToolConfig", which never hits
			// c.named) and a struct declared outside pkg/config -- and reflect flattens
			// both, so both are a real limit rather than a gap in the data. Neither
			// occurs in the tree today; the two emission lists are now supposed to agree,
			// so the limit is written down instead of being silently inherited by a port
			// that assumes the walk is complete.
			if !c.named[tn] {
				continue
			}
			// An embedded field with no json name is inlined. A tagged one is a
			// normal field; the diagnostics code checks the tag first, and no
			// embedded field in pkg/config carries one (verified: the dump has no
			// inlined type name as a key).
			if sub, ok := c.structs[tn]; ok {
				child := c.structNode(tn, sub, depth+1)
				for k, v := range child.Fields {
					out.Fields[k] = v
				}
				// yaml.v3 inlines an untagged anonymous struct the same way, so its
				// emission list splices in here -- unless the field is hidden.
				if _, _, _, hidden, _ := tagYAML(field.Tag, strings.TrimPrefix(tn, "*")); !hidden {
					out.YAML = append(out.YAML, child.YAML...)
				}
				// encoding/json inlines it the same way for the JSON document, and
				// there is no `json:"-"`-equivalent to hide it: the tag was already
				// checked for "-" above, so reaching here means the field is emitted.
				out.JSONFields = append(out.JSONFields, child.JSONFields...)
			}
			continue
		}
		tag := tagJSONName(field.Tag)
		if tag == "-" {
			continue
		}
		for _, ident := range field.Names {
			if !ast.IsExported(ident.Name) {
				continue
			}
			fieldName := tag
			if fieldName == "" {
				fieldName = ident.Name
			}
			// Cloned before it is annotated: nodeFor hands back a cached node for a named
			// type, and Omit describes the *field*, not the type. Two fields of the same
			// struct would otherwise overwrite each other's option.
			child := cloneNode(c.nodeFor(field.Type, depth+1))
			child.Omit = omitOption(field.Tag)
			out.Fields[fieldName] = child
			out.JSONFields = append(out.JSONFields, jsonField{
				JSON: fieldName,
				Omit: child.Omit,
			})
			yn, yInline, yOmit, yHidden, yUntagged := tagYAML(field.Tag, ident.Name)
			if !yHidden {
				out.YAML = append(out.YAML, yamlField{
					JSON:     fieldName,
					Name:     yn,
					Untagged: yUntagged,
					Inline:   yInline,
					Omit:     yOmit,
				})
			}
		}
	}
	// `Config` is the one node whose emission order is not its declaration order, and it
	// is the top of the document the whole list exists to describe, so it is modelled
	// rather than merely flagged. `Config.MarshalJSON` (config.go:252) marshals an
	// anonymous wrapper that declares `Session` as its own depth-0 field and embeds
	// `*Alias` beside it. typeFields takes depth-0 names first, so `session` wins the
	// name conflict with the promoted field *and* sorts ahead of every one of them:
	// Go's actual order is `session, version, isolation, agents, evolution,
	// channel_list, ...`. A port that wrote the declaration order would put `session`
	// fourth -- a different document to anything that compares bytes, in exactly the case
	// the wrapper exists for.
	//
	// The wrapper's `session,omitempty` is a *pointer*, so the omit rule here is Go's
	// pointer rule (nil is empty) and not the value-struct rule that keeps `isolation: {}`
	// in the file: MarshalJSON sets the pointer only when one of the three dimensions,
	// identity_links or dm_scope is non-empty.
	//
	// This runs exactly once per Config node because it sits on the single path that
	// builds `out`, and that path is also what stores `out` in the cache -- keep the
	// hoist there. Hoisting from a second site (a cache lookup, a post-pass over the
	// finished tree) would move `session` twice on a cached node and put it behind
	// `version`, which is the bug the hoist exists to prevent.
	if name == "Config" {
		for i, f := range out.JSONFields {
			if f.JSON == "session" {
				out.JSONFields = append(
					[]jsonField{f},
					append(out.JSONFields[:i:i], out.JSONFields[i+1:]...)...,
				)
				break
			}
		}
	}
	return out
}

func (c *ctx) nodeFor(expr ast.Expr, depth int) *node {
	switch e := strip(expr).(type) {
	case *ast.Ident:
		if st, ok := c.structs[e.Name]; ok {
			return c.structNode(e.Name, st, depth)
		}
		// A named map or slice (ChannelsConfig, ModelList, FlexibleStringSlice)
		// resolves through its declaration; reflect would see the same shape.
		if underlying, ok := c.aliases[e.Name]; ok && depth < 12 {
			n := c.nodeFor(underlying, depth+1)
			// Resolving the alias loses the name, and the name is what says whether
			// this position has a custom MarshalYAML: SecureModelList is a []*ModelConfig
			// underneath, but the security file writes the hand-built `name:index`
			// projection its MarshalYAML returns, not a sequence of model structs.
			// Carrying the flag across the alias is the only way the port can tell.
			// Annotated on a clone: nodeFor returns the cached node for the underlying
			// type, and these three fields describe the *alias*, not that type. Writing
			// through would mark every other position that resolves to the same shape.
			n = cloneNode(n)
			if c.yamlMarshalers[e.Name] {
				n.Marshaler = true
			}
			if c.jsonMarshalers[e.Name] {
				n.JSONMarshaler = true
			}
			// ...and the name too, for the same reason: `MarshalYAML` is declared on
			// SecureModelList, not on []*ModelConfig, so a port that dispatches on the
			// type it has to recognise the alias, not the shape underneath it.
			n.Name = e.Name
			return n
		}
		return &node{Kind: "scalar", Name: e.Name}
	case *ast.SelectorExpr:
		return &node{Kind: "scalar", Name: typeString(e)}
	case *ast.ArrayType:
		return &node{Kind: "slice", Elem: c.nodeFor(e.Elt, depth), Name: "[]" + typeString(e.Elt)}
	case *ast.MapType:
		return &node{Kind: "map", Elem: c.nodeFor(e.Value, depth), Name: "map[" + typeString(e.Key) + "]" + typeString(e.Value)}
	case *ast.StarExpr:
		return c.nodeFor(e.X, depth)
	case *ast.InterfaceType:
		return &node{Kind: "scalar", Name: "any"}
	default:
		return &node{Kind: "scalar", Name: typeString(expr)}
	}
}

// collectEnv walks the struct graph the way env.Parse does and appends one row per
// (variable, position) it would bind.
//
// The rules are the library's, read off env.go v11.4.1 rather than remembered, and
// each is a place a port could quietly differ:
//
//   - an unexported field is skipped: doParseField returns on !refField.CanSet().
//   - `env:"-"` prunes the field *and its subtree*: parseFieldParams sets Ignored and
//     doParseField returns before the struct recursion. Measured count in the commit.
//   - envPrefix on a field is prepended to everything below it and binds nothing
//     itself: optionsWithEnvPrefix is opts.Prefix + the tag.
//   - a struct field is walked whether or not it carries an `env` tag of its own
//     (env.go:407), which is why an absolute name inside a nested struct stays
//     absolute -- only envPrefix composes.
//   - a slice of structs is walked per element under `<prefix><i>_`, and only when
//     some variable carries that shape (doParseSlice counts the indices it finds).
//   - a map is never walked: doParseField has no Map case. That is the mechanism
//     behind what otherwise reads as a precedence rule --
//     PICOCLAW_CHANNELS_TELEGRAM_TOKEN changes nothing at all, because channel_list
//     is a map and a map value is not a field env ever sees.
func (c *ctx) collectEnv(structName string, prefix string, path []string, depth int, seen map[string]bool, out *[]envBinding) {
	key := structName + "\x00" + prefix + "\x00" + strings.Join(path, ".")
	if depth > 12 || seen[key] {
		return
	}
	st, ok := c.structs[structName]
	if !ok {
		return
	}
	seen[key] = true
	if c.fieldReachable == nil {
		c.fieldReachable = map[string]bool{}
	}
	c.fieldReachable[structName] = true
	for _, field := range st.Fields.List {
		envTag, hasEnv := tagValue(field.Tag, "env")
		prefixTag, _ := tagValue(field.Tag, "envPrefix")
		if envTag == "" && hasEnv {
			// env:"" would look up the empty variable name. None exist today; if one
			// is ever added, the index must not silently invent a binding for it.
			fmt.Fprintf(os.Stderr, "env:\"\" on a field of %s -- refusing to guess its name\n", structName)
			os.Exit(1)
		}
		if len(field.Names) == 0 {
			// embedded: encoding/json inlines it, so the path does not grow, while
			// reflect sees a promoted struct field whose prefix still applies.
			if envTag == "-" {
				continue
			}
			if name, ok := c.resolveStructName(field.Type); ok {
				c.collectEnv(name, prefix+prefixTag, path, depth+1, seen, out)
			}
			continue
		}
		for _, ident := range field.Names {
			if !ast.IsExported(ident.Name) {
				continue
			}
			if envTag == "-" {
				// Pruned, subtree included: the library returns before recursing.
				continue
			}
			jsonName := tagJSONName(field.Tag)
			if jsonName == "" {
				jsonName = ident.Name
			}
			if hasEnv {
				leaf := c.envLeafType(field.Type)
				row := envBinding{
					Var:  prefix + envTag,
					Path: append(append([]string{}, path...), jsonName),
					Go:   ident.Name,
					Type: leaf,
				}
				if strings.Contains(leaf, "SecureString") {
					row.Secret = true
				}
				if prefix != "" && strings.HasPrefix(envTag, prefix) {
					// Both halves are in the source; the loader composes them anyway.
					row.PrefixNote = "the env tag repeats the enclosing envPrefix, so the name read is both joined; the shorter name binds nothing"
				}
				if strings.HasSuffix(leaf, "RawMessage") {
					// Measured, not inferred from the library: the value is ParseUint'd
					// and written as a single raw byte into the raw-JSON field, so the
					// digits a user sets are not the digits they get. 49 lands as 1; 7
					// lands as byte 0x07 and the document then fails to marshal with
					// "invalid character '' looking for beginning of value". A port that
					// parsed this as JSON would load a file Go cannot save.
					row.ParseNote = "the parser reads an unsigned integer and writes it as one raw byte, not as JSON"
				}
				*out = append(*out, row)
			}
			c.descendEnv(field.Type, jsonName, prefix+prefixTag, path, depth, seen, out)
		}
	}
	delete(seen, key)
}

// resolveStructName names the struct a field's type leads to, following a named
// alias one step (`type AgentsConfig []AgentConfig` reaches AgentConfig).
func (c *ctx) resolveStructName(expr ast.Expr) (string, bool) {
	e, ok := strip(expr).(*ast.Ident)
	if !ok {
		return "", false
	}
	if _, isStruct := c.structs[e.Name]; isStruct {
		return e.Name, true
	}
	if underlying, ok := c.aliases[e.Name]; ok {
		return c.resolveStructName(underlying)
	}
	return "", false
}

// descendEnv recurses into a named field: structs under the accumulated prefix,
// slices of structs under the indexed prefix, maps not at all.
func (c *ctx) descendEnv(expr ast.Expr, jsonName, prefix string, path []string, depth int, seen map[string]bool, out *[]envBinding) {
	if name, ok := c.resolveStructName(expr); ok {
		c.collectEnv(name, prefix, append(append([]string{}, path...), jsonName), depth+1, seen, out)
		return
	}
	arr, ok := strip(expr).(*ast.ArrayType)
	if !ok {
		if underlying, isAlias := c.aliases[identName(expr)]; isAlias {
			c.descendEnv(underlying, jsonName, prefix, path, depth+1, seen, out)
		}
		return
	}
	elem, ok := strip(arr.Elt).(*ast.Ident)
	if !ok {
		return
	}
	if _, isStruct := c.structs[elem.Name]; !isStruct {
		return
	}
	// doParseSlice: element i is parsed under `<prefix><i>_`, and only if some
	// variable carries that shape. The rows are marked so a consumer splices the
	// index instead of treating the pattern name as a literal.
	before := len(*out)
	c.collectEnv(elem.Name, prefix, append(append([]string{}, path...), jsonName, "0"), depth+1, seen, out)
	for i := before; i < len(*out); i++ {
		(*out)[i].Indexed = true
	}
}

// identName is the alias key for a type expression, or "" when it is not a plain
// named type -- a map or an anonymous struct has no env-relevant alias to follow.
func identName(expr ast.Expr) string {
	if e, ok := strip(expr).(*ast.Ident); ok {
		return e.Name
	}
	return ""
}

// envLeafType names the type the parser targets, because that is what decides
// whether a value is mistyped. A named type reports `Name:underlying`.
func (c *ctx) envLeafType(expr ast.Expr) string {
	switch e := strip(expr).(type) {
	case *ast.Ident:
		if underlying, ok := c.aliases[e.Name]; ok {
			if inner := c.envLeafType(underlying); inner != "" {
				return e.Name + ":" + inner
			}
		}
		return e.Name
	case *ast.ArrayType:
		return "[]" + typeString(e.Elt)
	default:
		return typeString(expr)
	}
}

// collectShims finds every os.LookupEnv / os.Getenv call in the package and names
// what it reads. This is the compat-shim inventory: unlike the tags there is
// nothing to walk, so the call sites are the list, and each row carries the
// enclosing function as its provenance.
//
// An argument that is neither a string literal nor a resolvable constant is an
// error rather than a skip: a missing row tells a port that a variable is strict
// when it is lenient, and that is the silent direction of failure.
func (c *ctx) collectShims(tagged map[string]bool) []envShim {
	type ref struct {
		expr ast.Expr
		fn   string
	}
	var refs []ref
	for _, f := range c.files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := fd.Name.Name
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || len(call.Args) != 1 {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "os" {
					return true
				}
				if sel.Sel.Name != "LookupEnv" && sel.Sel.Name != "Getenv" {
					return true
				}
				refs = append(refs, ref{call.Args[0], fn})
				return true
			})
		}
	}
	byVar := map[string]envShim{}
	for _, r := range refs {
		name := constName(r.expr, c.consts)
		if name == "" {
			if lit, ok := r.expr.(*ast.BasicLit); ok {
				name, _ = strconv.Unquote(lit.Value)
			}
		}
		if name == "" {
			fmt.Fprintf(os.Stderr, "%s: cannot resolve the environment name %s\n", r.fn, typeString(r.expr))
			os.Exit(1)
		}
		row := byVar[name]
		if row.Var == "" {
			row = envShim{Var: name, Reader: r.fn, Tagged: tagged[name]}
		} else if !row.Tagged {
			row.Tagged = tagged[name]
		}
		byVar[name] = row
	}
	var out []envShim
	for _, row := range byVar {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Var < out[j].Var })
	return out
}

// sortedKeys makes the channel pass deterministic: it iterates a map of channel
// types, and the dump is embedded and compared byte for byte.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tagValue reads one named struct tag out of a field's tag literal, which is a raw
// literal everywhere in this package but is unquoted properly in case that changes.
func tagValue(tag *ast.BasicLit, key string) (string, bool) {
	if tag == nil {
		return "", false
	}
	text := strings.Trim(tag.Value, "`")
	if text == tag.Value {
		// a double-quoted tag literal rather than a raw one
		if u, err := strconv.Unquote(tag.Value); err == nil {
			text = u
		}
	}
	v, ok := reflect.StructTag(text).Lookup(key)
	return v, ok
}

func strip(expr ast.Expr) ast.Expr {
	for {
		star, ok := expr.(*ast.StarExpr)
		if !ok {
			return expr
		}
		expr = star.X
	}
}

// cloneNode copies a node before per-field annotation. Children stay shared: they are
// only ever read after the walk finishes.
func cloneNode(n *node) *node {
	c := *n
	// The emission lists are copied rather than shared. nodeFor hands back one cached
	// node per named type, so a shallow clone plus an append writes into the cached
	// node's backing array and every later reader of that type sees the extra entry --
	// the same class of bug as annotating Omit on a shared node, which used to make two
	// fields of one struct overwrite each other's option.
	if n.YAML != nil {
		c.YAML = append([]yamlField(nil), n.YAML...)
	}
	if n.JSONFields != nil {
		c.JSONFields = append([]jsonField(nil), n.JSONFields...)
	}
	// Outside the JSONFields guard on purpose: `c := *n` already carried the bool, but
	// reading it inside the guard says the flag only clones on nodes that have a JSON
	// list, and the flagged nodes that most need it -- SecureString's 36 -- have
	// `json_fields == nil`.
	c.JSONMarshaler = n.JSONMarshaler
	return &c
}

// constName reads a map key that is either a string literal or a named constant.
func constName(expr ast.Expr, consts map[string]string) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if v, err := strconv.Unquote(e.Value); err == nil {
			return v
		}
	case *ast.Ident:
		return consts[e.Name]
	}
	return ""
}

// structOf reads the type name out of a `FooSettings{}` or `&FooSettings{}` literal.
func structOf(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.UnaryExpr:
			expr = e.X
		case *ast.ParenExpr: // the factory writes `(TelegramSettings{})`
			expr = e.X
		default:
			goto build
		}
	}
build:
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return ""
	}
	return typeString(lit.Type)
}

// collectKeys reads the keys of a `map[string]struct{}{ "a": {}, ... }` literal.
func collectKeys(expr ast.Expr, into *[]string) {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if bl, ok := kv.Key.(*ast.BasicLit); ok {
			if v, err := strconv.Unquote(bl.Value); err == nil {
				*into = append(*into, v)
			}
		}
	}
	sort.Strings(*into)
}

// receiverName is the base type name of a method receiver, so `func (v SecureModelList)`
// and `func (b Channel)` both resolve to the struct name the dump keys on.
func receiverName(fl *ast.FieldList) string {
	if len(fl.List) == 0 {
		return ""
	}
	return typeString(strip(fl.List[0].Type))
}

func tagJSONName(tag *ast.BasicLit) string {
	if tag == nil {
		return ""
	}
	v, err := strconv.Unquote(tag.Value)
	if err != nil {
		return ""
	}
	return strings.Split(reflect.StructTag(v).Get("json"), ",")[0]
}

func typeString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + typeString(e.X)
	case *ast.ArrayType:
		return "[]" + typeString(e.Elt)
	case *ast.SelectorExpr:
		return typeString(e.X) + "." + e.Sel.Name
	case *ast.MapType:
		return "map[" + typeString(e.Key) + "]" + typeString(e.Value)
	case *ast.InterfaceType:
		return "any"
	default:
		return "?"
	}
}

var _ = sort.Strings
