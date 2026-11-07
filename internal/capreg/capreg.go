// Package capreg is the code-owned semantic and policy registry for bot
// capabilities. It owns language (aliases, examples, negatives) and bot-facing
// policy metadata (tier, confirmation, slot roles, receipts). It never owns an
// executable recipe: handlers, operations and console scripts stay in Go.
package capreg

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed catalog.yaml
var catalogYAML []byte

// Tier is the bot-facing risk tier. It decides confirmation policy only; it
// never widens visibility or execution permission.
type Tier string

const (
	TierRead       Tier = "R0" // reads
	TierReversible Tier = "R1" // single player, reversible
	TierRoomWide   Tier = "R2" // room-wide or affects other players
	TierDestroy    Tier = "R3" // destructive or irreversible
)

// Confirm is the confirmation the decision layer must obtain before executing.
type Confirm string

const (
	ConfirmNone  Confirm = "none"
	ConfirmOnce  Confirm = "once"
	ConfirmTyped Confirm = "typed"
)

// Slot roles describe what a parameter means to the understanding layer.
var slotRoles = map[string]bool{
	"target_player": true, "other_player": true, "world": true, "item": true,
	"entity": true, "quantity": true, "toggle": true, "time_range": true,
	"text": true, "location": true, "enum": true, "presentation": true, "internal": true,
}

// Ask policies: if_missing asks the user only when the value cannot be
// extracted and has no default; never means the value is defaulted, omitted or
// filled by the server and must not produce a prompt.
var askPolicies = map[string]bool{"if_missing": true, "never": true}

// Slot is the resolved role metadata for one capability parameter.
type Slot struct {
	Role  string `json:"role"`
	Ask   string `json:"ask"`
	Model bool   `json:"model"`
}

type slotSpec struct {
	Role  string `yaml:"role"`
	Ask   string `yaml:"ask"`
	Model *bool  `yaml:"model"`
}

// Receipt holds user-facing receipt templates. Placeholders use {slot} names.
type Receipt struct {
	Done    string `yaml:"done" json:"done,omitempty"`
	Pending string `yaml:"pending" json:"pending,omitempty"`
}

// Action is the language and policy of one enum sub-action. Empty tier or
// confirm inherit from the parent capability.
type Action struct {
	Aliases   []string `yaml:"aliases" json:"aliases,omitempty"`
	Examples  []string `yaml:"examples" json:"examples,omitempty"`
	Negatives []string `yaml:"negatives" json:"negatives,omitempty"`
	Tier      Tier     `yaml:"tier" json:"tier,omitempty"`
	Confirm   Confirm  `yaml:"confirm" json:"confirm,omitempty"`
	Prefer    string   `yaml:"prefer" json:"prefer,omitempty"`
}

// Entry is one capability record.
type Entry struct {
	ID        string              `yaml:"id"`
	Aliases   []string            `yaml:"aliases"`
	Examples  []string            `yaml:"examples"`
	Negatives []string            `yaml:"negatives"`
	Tier      Tier                `yaml:"tier"`
	Confirm   Confirm             `yaml:"confirm"`
	Prefer    string              `yaml:"prefer"`
	Receipt   *Receipt            `yaml:"receipt"`
	Actions   map[string]Action   `yaml:"actions"`
	RawSlots  map[string]slotSpec `yaml:"slots"`
	slots     map[string]Slot
	defaults  map[string]Slot
}

// Slot resolves the role of a parameter: the entry's own declaration wins over
// the catalog-wide defaults for common parameter names.
func (e Entry) Slot(name string) (Slot, bool) {
	if s, ok := e.slots[name]; ok {
		return s, true
	}
	s, ok := e.defaults[name]
	return s, ok
}

// Slots returns the entry's explicitly declared slots (without defaults).
func (e Entry) Slots() map[string]Slot {
	out := make(map[string]Slot, len(e.slots))
	for k, v := range e.slots {
		out[k] = v
	}
	return out
}

// Catalog is a validated registry.
type Catalog struct {
	entries  []Entry
	byID     map[string]int
	defaults map[string]Slot
}

type document struct {
	SlotDefaults map[string]slotSpec `yaml:"slot_defaults"`
	Capabilities []Entry             `yaml:"capabilities"`
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
var actionPattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// ErrInvalid reports a catalog that violates the schema.
var ErrInvalid = errors.New("capreg: invalid catalog")

func resolveSlot(where string, s slotSpec) (Slot, error) {
	if !slotRoles[s.Role] {
		return Slot{}, fmt.Errorf("%w: %s: unknown role %q", ErrInvalid, where, s.Role)
	}
	out := Slot{Role: s.Role, Ask: s.Ask, Model: true}
	if out.Ask == "" {
		out.Ask = "if_missing"
		if s.Role == "presentation" || s.Role == "internal" || s.Role == "world" {
			out.Ask = "never"
		}
	}
	if !askPolicies[out.Ask] {
		return Slot{}, fmt.Errorf("%w: %s: unknown ask %q", ErrInvalid, where, s.Ask)
	}
	if s.Model != nil {
		out.Model = *s.Model
	} else if s.Role == "presentation" || s.Role == "internal" {
		out.Model = false
	}
	if (s.Role == "presentation" || s.Role == "internal") && (out.Model || out.Ask != "never") {
		// Presentation and protocol fields are never asked and never shown to a model.
		return Slot{}, fmt.Errorf("%w: %s: %s slot must be ask=never model=false", ErrInvalid, where, s.Role)
	}
	return out, nil
}

func validPolicy(where string, tier Tier, confirm Confirm) error {
	switch tier {
	case TierRead, TierReversible, TierRoomWide, TierDestroy:
	default:
		return fmt.Errorf("%w: %s: unknown tier %q", ErrInvalid, where, tier)
	}
	switch confirm {
	case ConfirmNone, ConfirmOnce, ConfirmTyped:
	default:
		return fmt.Errorf("%w: %s: unknown confirm %q", ErrInvalid, where, confirm)
	}
	// Invariant I6: confirmation is policy, and policy must be monotone in risk.
	if tier == TierRead && confirm != ConfirmNone {
		return fmt.Errorf("%w: %s: R0 reads cannot require confirmation", ErrInvalid, where)
	}
	if tier == TierRoomWide && confirm == ConfirmNone {
		return fmt.Errorf("%w: %s: R2 requires confirmation", ErrInvalid, where)
	}
	if tier == TierDestroy && confirm != ConfirmTyped {
		return fmt.Errorf("%w: %s: R3 requires typed confirmation", ErrInvalid, where)
	}
	return nil
}

func cleanList(where, field string, in []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || len([]rune(v)) > 128 {
			return nil, fmt.Errorf("%w: %s: bad %s %q", ErrInvalid, where, field, v)
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out, nil
}

// Parse validates a catalog document. Unknown fields are rejected so a typo in
// a policy key cannot silently fall back to a weaker default.
func Parse(raw []byte) (*Catalog, error) {
	var doc document
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	c := &Catalog{byID: map[string]int{}, defaults: map[string]Slot{}}
	for name, spec := range doc.SlotDefaults {
		s, err := resolveSlot("slot_defaults."+name, spec)
		if err != nil {
			return nil, err
		}
		c.defaults[name] = s
	}
	for _, e := range doc.Capabilities {
		if !idPattern.MatchString(e.ID) {
			return nil, fmt.Errorf("%w: bad id %q", ErrInvalid, e.ID)
		}
		if _, dup := c.byID[e.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate id %q", ErrInvalid, e.ID)
		}
		var err error
		if e.Aliases, err = cleanList(e.ID, "alias", e.Aliases); err != nil {
			return nil, err
		}
		if e.Examples, err = cleanList(e.ID, "example", e.Examples); err != nil {
			return nil, err
		}
		if e.Negatives, err = cleanList(e.ID, "negative", e.Negatives); err != nil {
			return nil, err
		}
		if err = validPolicy(e.ID, e.Tier, e.Confirm); err != nil {
			return nil, err
		}
		if e.Prefer != "" && (!idPattern.MatchString(e.Prefer) || e.Prefer == e.ID) {
			return nil, fmt.Errorf("%w: %s: bad prefer %q", ErrInvalid, e.ID, e.Prefer)
		}
		e.slots = map[string]Slot{}
		for name, spec := range e.RawSlots {
			s, err := resolveSlot(e.ID+".slots."+name, spec)
			if err != nil {
				return nil, err
			}
			e.slots[name] = s
		}
		e.RawSlots = nil
		e.defaults = c.defaults
		for key, a := range e.Actions {
			where := e.ID + ".actions." + key
			if !actionPattern.MatchString(key) {
				return nil, fmt.Errorf("%w: bad action key %q", ErrInvalid, where)
			}
			if a.Tier == "" {
				a.Tier = e.Tier
			}
			if a.Confirm == "" {
				a.Confirm = e.Confirm
			}
			if err = validPolicy(where, a.Tier, a.Confirm); err != nil {
				return nil, err
			}
			if a.Aliases, err = cleanList(where, "alias", a.Aliases); err != nil {
				return nil, err
			}
			if a.Examples, err = cleanList(where, "example", a.Examples); err != nil {
				return nil, err
			}
			if a.Negatives, err = cleanList(where, "negative", a.Negatives); err != nil {
				return nil, err
			}
			if a.Prefer != "" && !idPattern.MatchString(a.Prefer) {
				return nil, fmt.Errorf("%w: %s: bad prefer %q", ErrInvalid, where, a.Prefer)
			}
			e.Actions[key] = a
		}
		c.byID[e.ID] = len(c.entries)
		c.entries = append(c.entries, e)
	}
	sort.SliceStable(c.entries, func(i, j int) bool { return c.entries[i].ID < c.entries[j].ID })
	for i, e := range c.entries {
		c.byID[e.ID] = i
	}
	return c, nil
}

// All returns every entry sorted by ID.
func (c *Catalog) All() []Entry { return append([]Entry(nil), c.entries...) }

// Lookup returns one entry by capability ID.
func (c *Catalog) Lookup(id string) (Entry, bool) {
	i, ok := c.byID[id]
	if !ok {
		return Entry{}, false
	}
	return c.entries[i], true
}

// SlotDefaults returns catalog-wide default roles for common parameter names.
func (c *Catalog) SlotDefaults() map[string]Slot {
	out := make(map[string]Slot, len(c.defaults))
	for k, v := range c.defaults {
		out[k] = v
	}
	return out
}

var (
	loadOnce sync.Once
	builtin  *Catalog
	loadErr  error
)

// Builtin returns the embedded catalog. The catalog is code-owned and covered
// by tests; an invalid embedded catalog is a build defect and panics.
func Builtin() *Catalog {
	loadOnce.Do(func() { builtin, loadErr = Parse(catalogYAML) })
	if loadErr != nil {
		panic(loadErr)
	}
	return builtin
}

// All returns every embedded entry sorted by ID.
func All() []Entry { return Builtin().All() }

// Lookup returns one embedded entry.
func Lookup(id string) (Entry, bool) { return Builtin().Lookup(id) }
