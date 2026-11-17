package mask

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/value"
)

// Row is one row of the state index: a managed resource and the attributes
// the index keeps.
type Row struct {
	Address  string            `json:"address"`
	Type     string            `json:"type"`
	Provider string            `json:"provider"`
	Attrs    map[string]string `json:"attrs"`
}

// rawState is the part of `tofu show -json` the mask reads.
type rawState struct {
	FormatVersion string `json:"format_version"`
	Values        *struct {
		Outputs map[string]struct {
			Sensitive bool `json:"sensitive"`
			Value     any  `json:"value"`
		} `json:"outputs"`
		RootModule rawStateModule `json:"root_module"`
	} `json:"values"`
}

type rawStateModule struct {
	Resources []struct {
		Address         string         `json:"address"`
		Mode            string         `json:"mode"`
		Type            string         `json:"type"`
		ProviderName    string         `json:"provider_name"`
		Values          map[string]any `json:"values"`
		SensitiveValues any            `json:"sensitive_values"`
	} `json:"resources"`
	ChildModules []rawStateModule `json:"child_modules"`
}

// Index reads the output of `tofu show -json` of a state and returns its
// rows, sorted by address. A row keeps only the attributes that attrs names
// for its resource type, that hold a scalar, that the state does not mark
// sensitive, and whose names do not say secret. Index learns the state's
// secrets: its sensitive outputs and every value under a mark.
func (m *Mask) Index(raw []byte, attrs func(typ string) []string) ([]Row, error) {
	var st rawState
	if err := decode(raw, &st); err != nil {
		return nil, fmt.Errorf("read the state: %w", err)
	}
	if err := m.learnState(&st); err != nil {
		return nil, err
	}
	if st.Values == nil {
		return []Row{}, nil
	}
	rows := []Row{}
	var walk func(mod rawStateModule)
	walk = func(mod rawStateModule) {
		for _, r := range mod.Resources {
			if r.Mode != "managed" {
				continue
			}
			row := Row{Address: r.Address, Type: r.Type, Provider: r.ProviderName, Attrs: map[string]string{}}
			for _, a := range attrs(r.Type) {
				if value.DeniedName(a) || r.Values[a] == Sensitive {
					continue
				}
				if s, ok := scalar(r.Values[a]); ok {
					row.Attrs[a] = s
				}
			}
			rows = append(rows, row)
		}
		for _, c := range mod.ChildModules {
			walk(c)
		}
	}
	walk(st.Values.RootModule)
	s, err := m.scan()
	if err != nil {
		return nil, err
	}
	s.walk(reflect.ValueOf(rows))
	if s.err != nil {
		return nil, s.err
	}
	slices.SortFunc(rows, func(a, b Row) int { return strings.Compare(a.Address, b.Address) })
	return rows, nil
}

// learnState collects the sensitive outputs of a state and every value under
// a resource's marks, and replaces each such value with Sensitive. A true
// mark on a whole resource leaves it no values.
func (m *Mask) learnState(st *rawState) error {
	if err := checkFormat(st.FormatVersion); err != nil {
		return err
	}
	if st.Values == nil {
		return nil
	}
	for _, o := range st.Values.Outputs {
		if o.Sensitive {
			m.collect(o.Value)
		}
	}
	i := 0
	var walk func(mod rawStateModule) error
	walk = func(mod rawStateModule) error {
		for j := range mod.Resources {
			r := &mod.Resources[j]
			v, err := m.redact(r.Values, r.SensitiveValues, nil)
			if err != nil {
				return fmt.Errorf("resource %d: sensitive_values: %w", i, err)
			}
			r.Values, _ = v.(map[string]any)
			i++
		}
		for _, c := range mod.ChildModules {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(st.Values.RootModule)
}

func scalar(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case bool:
		return strconv.FormatBool(v), true
	}
	return "", false
}
