package codegen

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is generator_config.yml: tfplugingen-openapi's generator config
// format plus keys only sfcgen reads. It is parsed strictly, so a typo is an
// error rather than a silently ignored key.
type Config struct {
	Provider    ProviderConfig              `yaml:"provider"`
	Derive      DeriveConfig                `yaml:"derive"`
	Skip        SkipConfig                  `yaml:"skip"`
	Resources   map[string]ResourceConfig   `yaml:"resources"`
	DataSources map[string]DataSourceConfig `yaml:"data_sources"`
}

// ProviderConfig mirrors tfplugingen-openapi's provider section.
type ProviderConfig struct {
	Name      string   `yaml:"name"`
	SchemaRef string   `yaml:"schema_ref,omitempty"`
	Ignores   []string `yaml:"ignores,omitempty"`
}

// DeriveConfig controls deriving entries from REST path conventions.
type DeriveConfig struct {
	Resources   bool `yaml:"resources"`
	DataSources bool `yaml:"data_sources"`
	// Prefixes are the API version prefixes to look under, in priority
	// order: when /v2/things and /preview/v2/things both exist, the first
	// listed prefix wins.
	Prefixes []string `yaml:"prefixes"`
}

// SkipConfig lists derived names that must not be generated, with reasons.
type SkipConfig struct {
	Resources   map[string]string `yaml:"resources"`
	DataSources map[string]string `yaml:"data_sources"`
}

// Location is an operation reference.
type Location struct {
	Path   string `yaml:"path"`
	Method string `yaml:"method"`
}

func (l *Location) String() string {
	if l == nil {
		return "<none>"
	}
	return strings.ToUpper(l.Method) + " " + l.Path
}

// SchemaOptions mirrors tfplugingen-openapi's schema options.
type SchemaOptions struct {
	Ignores    []string         `yaml:"ignores,omitempty"`
	Attributes AttributeOptions `yaml:"attributes,omitempty"`
}

// AttributeOptions mirrors tfplugingen-openapi's attribute options.
type AttributeOptions struct {
	Aliases   map[string]string   `yaml:"aliases,omitempty"`
	Overrides map[string]Override `yaml:"overrides,omitempty"`
}

// Override mirrors tfplugingen-openapi's attribute override.
type Override struct {
	Description string `yaml:"description,omitempty"`
}

// ResourceConfig is one resources.<name> entry.
type ResourceConfig struct {
	Create *Location      `yaml:"create,omitempty"`
	Read   *Location      `yaml:"read,omitempty"`
	Update *Location      `yaml:"update,omitempty"`
	Delete *Location      `yaml:"delete,omitempty"`
	Schema *SchemaOptions `yaml:"schema,omitempty"`

	// sfcgen only.

	// Spending marks a resource whose creation spends money or consumes
	// purchased compute. Such resources can only be created when the
	// provider has allow_spending = true. Unset means true: a resource
	// nobody has classified is assumed to cost money.
	Spending *bool `yaml:"spending,omitempty"`
	// SpendingNote explains the cost in the error shown when blocked.
	SpendingNote string `yaml:"spending_note,omitempty"`
	// BeforeDelete operations run, in order, before Delete (for example
	// terminating an instance before deleting it). 404 and 409 responses
	// from them are ignored.
	BeforeDelete []Location `yaml:"before_delete,omitempty"`
	// DeleteAcceptStatus lists HTTP statuses (besides 404) from the delete
	// operation that mean there is nothing left to delete, e.g. 422 "order
	// cannot be cancelled" for an order that already filled.
	DeleteAcceptStatus []int `yaml:"delete_accept_status,omitempty"`
	// Stable lists computed attributes that never change after creation,
	// so plans keep their prior value instead of showing "known after
	// apply" on every update. Defaults to id and created_at.
	Stable []string `yaml:"stable,omitempty"`
}

// DataSourceConfig is one data_sources.<name> entry.
type DataSourceConfig struct {
	Read   *Location      `yaml:"read,omitempty"`
	Schema *SchemaOptions `yaml:"schema,omitempty"`
}

// ParseConfig parses generator_config.yml strictly.
func ParseConfig(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("generator config: %w", err)
	}
	if c.Provider.Name == "" {
		return nil, fmt.Errorf("generator config: provider.name is required")
	}
	if len(c.Derive.Prefixes) == 0 {
		c.Derive.Prefixes = []string{"/v2", "/preview/v2"}
	}
	for name, r := range c.Resources {
		for _, l := range []*Location{r.Create, r.Read, r.Update, r.Delete} {
			if l != nil && (l.Path == "" || l.Method == "") {
				return nil, fmt.Errorf("generator config: resources.%s: operations need both path and method", name)
			}
		}
	}
	return &c, nil
}
