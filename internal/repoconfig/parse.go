// Package repoconfig reads the untrusted .codefabric/project.yaml of a repo and
// merges it over the configuration of the Controller (§3.1). It is pure: it
// reads no repo and no filesystem, so that the Controller (to start a Run and
// to explain it) and the Worker (to run it) reach the same effective
// configuration from the same bytes.
package repoconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Path is where a repo keeps its configuration, relative to its root.
const Path = ".codefabric/project.yaml"

// MaxFileBytes caps the file: it comes from the repo, so it is hostile input.
const MaxFileBytes = 64 << 10

const maxDetail = 200

// File is what a repo may say about itself (§3.1). A nil field was not said;
// a said field replaces the one of the Controller as a whole.
type File struct {
	// Evaluation is recorded and shown; nothing runs it yet.
	Evaluation  map[string]any
	Environment *EnvironmentOverride
	Resources   *ResourcesOverride
}

// EnvironmentOverride holds the fields of an Environment a repo may set. The
// provider and the capabilities it requires belong to the Controller.
type EnvironmentOverride struct {
	Prepare      *string
	Services     *[]domain.EnvironmentService
	DigestInputs *[]string
}

// ResourcesOverride is what the repo asks for; the Controller's value is a
// ceiling.
type ResourcesOverride struct {
	CPU      *int
	MemoryMB *int
}

type rawFile struct {
	Evaluation  map[string]any  `yaml:"evaluation"`
	Environment *rawEnvironment `yaml:"environment"`
	Defaults    *rawDefaults    `yaml:"defaults"`
}

type rawDefaults struct {
	Resources *rawResources `yaml:"resources"`
}

type rawResources struct {
	CPU      *int `yaml:"cpu"`
	MemoryMB *int `yaml:"memory_mb"`
}

type rawEnvironment struct {
	Prepare      *string       `yaml:"prepare"`
	Services     *[]rawService `yaml:"services"`
	DigestInputs *[]string     `yaml:"digest_inputs"`
}

type rawService struct {
	Name  string `yaml:"name"`
	Start string `yaml:"start"`
	Stop  string `yaml:"stop"`
	Ready string `yaml:"ready"`
}

// shape is the set of keys a mapping may carry; items shapes the mappings of a
// list. A nil shape is a free value.
type shape struct {
	keys  map[string]*shape
	items *shape
}

var fileShape = &shape{keys: map[string]*shape{
	"evaluation": nil,
	"environment": {keys: map[string]*shape{
		"prepare":       nil,
		"digest_inputs": nil,
		"services": {items: &shape{keys: map[string]*shape{
			"name": nil, "start": nil, "stop": nil, "ready": nil,
		}}},
	}},
	"defaults": {keys: map[string]*shape{
		"resources": {keys: map[string]*shape{"cpu": nil, "memory_mb": nil}},
	}},
}}

// Parse reads the file strictly: one document, no anchors or aliases, only the
// keys a repo may set, and values that make a valid configuration. Anything
// else is CF_REPO_CONFIG_INVALID naming the offending key. An empty file says
// nothing.
func Parse(b []byte) (File, error) {
	if len(b) > MaxFileBytes {
		return File{}, invalid("", "file is larger than %d bytes", MaxFileBytes)
	}
	root, err := readNode(b)
	if err != nil {
		return File{}, err
	}
	if root == nil {
		return File{}, nil
	}
	if err := checkNode(root, fileShape, ""); err != nil {
		return File{}, err
	}
	var raw rawFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return File{}, invalid("", "%s", err.Error())
	}
	return build(raw)
}

// readNode returns the content of the only document, or nil if there is none.
// Aliases and anchors are refused before anything is decoded into values, which
// is where an expansion bomb would go off.
func readNode(b []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, invalid("", "%s", err.Error())
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, invalid("", "more than one document")
	} else if !errors.Is(err, io.EOF) {
		return nil, invalid("", "%s", err.Error())
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	return doc.Content[0], nil
}

func checkNode(n *yaml.Node, s *shape, path string) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return invalid(path, "anchors and aliases are not allowed")
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			key := join(path, k.Value)
			if k.Kind == yaml.AliasNode || k.Anchor != "" {
				return invalid(key, "anchors and aliases are not allowed")
			}
			if s == nil {
				if err := checkNode(v, nil, key); err != nil {
					return err
				}
				continue
			}
			sub, ok := s.keys[k.Value]
			if !ok {
				return invalid(key, "key is not allowed in a repo configuration")
			}
			if err := checkNode(v, sub, key); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			var itemShape *shape
			if s != nil {
				itemShape = s.items
			}
			if err := checkNode(item, itemShape, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func build(raw rawFile) (File, error) {
	f := File{Evaluation: raw.Evaluation}
	if f.Evaluation != nil {
		if _, err := json.Marshal(f.Evaluation); err != nil {
			return File{}, invalid("evaluation", "keys must be strings")
		}
	}
	if raw.Defaults != nil && raw.Defaults.Resources != nil {
		r := raw.Defaults.Resources
		if r.CPU != nil && *r.CPU <= 0 {
			return File{}, invalid("defaults.resources.cpu", "must be greater than zero")
		}
		if r.MemoryMB != nil && *r.MemoryMB <= 0 {
			return File{}, invalid("defaults.resources.memory_mb", "must be greater than zero")
		}
		f.Resources = &ResourcesOverride{CPU: r.CPU, MemoryMB: r.MemoryMB}
	}
	if raw.Environment != nil {
		env, err := buildEnvironment(*raw.Environment)
		if err != nil {
			return File{}, err
		}
		f.Environment = env
	}
	return f, nil
}

// buildEnvironment applies the rules of the domain to the fields the repo set,
// so that a repo cannot say what an Environment of the Controller could not.
func buildEnvironment(raw rawEnvironment) (*EnvironmentOverride, error) {
	o := &EnvironmentOverride{Prepare: raw.Prepare, DigestInputs: raw.DigestInputs}
	if raw.Services != nil {
		services := make([]domain.EnvironmentService, 0, len(*raw.Services))
		for _, s := range *raw.Services {
			services = append(services, domain.EnvironmentService(s))
		}
		o.Services = &services
	}
	probe := domain.EnvironmentDef{Provider: domain.ProviderHost}
	if o.Prepare != nil {
		probe.Prepare = *o.Prepare
	}
	if o.Services != nil {
		probe.Services = *o.Services
	}
	if o.DigestInputs != nil {
		probe.DigestInputs = *o.DigestInputs
	}
	if err := probe.Validate(); err != nil {
		return nil, invalid("environment", "%s", strings.TrimPrefix(err.Error(), domain.ErrInvalidEnvironment.Error()+": "))
	}
	return o, nil
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// invalid is CF_REPO_CONFIG_INVALID; key is the dotted path of the offending
// key, and both it and the detail are cut to a bounded length because they
// come from the repo.
func invalid(key, format string, args ...any) *domain.Error {
	detail := bound(fmt.Sprintf(format, args...))
	key = bound(key)
	msg := detail
	details := map[string]any{}
	if key != "" {
		msg = fmt.Sprintf("%s: %s", key, detail)
		details["key"] = key
	}
	return domain.NewError(domain.CodeRepoConfigInvalid, "repo configuration: "+msg, details)
}

func bound(s string) string {
	s = strings.ToValidUTF8(s, "?")
	if len(s) > maxDetail {
		return s[:maxDetail] + "..."
	}
	return s
}
