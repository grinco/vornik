package apigateway

import (
	"errors"
	"reflect"
	"testing"
)

func TestMethodAllowed_ReadOnlyDefault(t *testing.T) {
	ro := Provider{BasePath: "/maps", AllowedMethods: []string{"GET"}, WritesEnabled: false}
	for _, m := range []string{"GET", "get", "HEAD"} {
		if err := MethodAllowed(ro, m); err != nil {
			t.Errorf("MethodAllowed(%q) = %v, want nil (read allowed)", m, err)
		}
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if err := MethodAllowed(ro, m); !errors.Is(err, ErrMethodNotAllowed) {
			t.Errorf("MethodAllowed(%q) = %v, want ErrMethodNotAllowed (writes off)", m, err)
		}
	}
}

func TestMethodAllowed_WritesOptIn(t *testing.T) {
	rw := Provider{AllowedMethods: []string{"GET", "POST"}, WritesEnabled: true}
	if err := MethodAllowed(rw, "POST"); err != nil {
		t.Errorf("POST with writes_enabled+listed = %v, want nil", err)
	}
	// A write method NOT in AllowedMethods is still refused even with writes on.
	if err := MethodAllowed(rw, "DELETE"); !errors.Is(err, ErrMethodNotAllowed) {
		t.Errorf("DELETE not in AllowedMethods = %v, want ErrMethodNotAllowed", err)
	}
}

func TestRegistry_Lookup(t *testing.T) {
	r := Registry{"maps": {BasePath: "/maps"}}
	if _, ok := r.Lookup("maps"); !ok {
		t.Error("expected maps found")
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Error("expected nope not found")
	}
}

func TestProvider_Examples_RoundTrip(t *testing.T) {
	p := Provider{
		BasePath:       "/weather",
		AllowedMethods: []string{"GET"},
		WritesEnabled:  false,
		Description:    "Weather API",
		Examples:       []string{"GET /weather/current?city=Prague", "GET /weather/forecast?city=Brno"},
	}
	want := []string{"GET /weather/current?city=Prague", "GET /weather/forecast?city=Brno"}
	if !reflect.DeepEqual(p.Examples, want) {
		t.Errorf("Provider.Examples = %v, want %v", p.Examples, want)
	}
}

func TestDescribe_SortedByNameWithAllFields(t *testing.T) {
	r := Registry{
		"weather": {
			BasePath:       "/weather",
			AllowedMethods: []string{"GET"},
			WritesEnabled:  false,
			Description:    "Weather API",
			Examples:       []string{"GET /weather/current"},
		},
		"maps": {
			BasePath:       "/maps",
			AllowedMethods: []string{"GET", "POST"},
			WritesEnabled:  true,
			Description:    "Maps API",
			Examples:       []string{"GET /maps/geocode", "POST /maps/route"},
		},
		"billing": {
			BasePath:       "/billing",
			AllowedMethods: []string{"GET"},
			WritesEnabled:  false,
			Description:    "Billing API",
			Examples:       nil,
		},
	}

	got := r.Describe()

	want := []ProviderInfo{
		{
			Name:           "billing",
			Description:    "Billing API",
			AllowedMethods: []string{"GET"},
			WritesEnabled:  false,
			Examples:       nil,
		},
		{
			Name:           "maps",
			Description:    "Maps API",
			AllowedMethods: []string{"GET", "POST"},
			WritesEnabled:  true,
			Examples:       []string{"GET /maps/geocode", "POST /maps/route"},
		},
		{
			Name:           "weather",
			Description:    "Weather API",
			AllowedMethods: []string{"GET"},
			WritesEnabled:  false,
			Examples:       []string{"GET /weather/current"},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Describe() = %#v, want %#v", got, want)
	}

	for i := 1; i < len(got); i++ {
		if got[i-1].Name >= got[i].Name {
			t.Errorf("Describe() not sorted by Name: %q before %q", got[i-1].Name, got[i].Name)
		}
	}
}

func TestDescribe_EmptyRegistry(t *testing.T) {
	r := Registry{}
	got := r.Describe()
	if got == nil {
		t.Error("Describe() on empty registry returned nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("Describe() on empty registry = %v, want empty", got)
	}
}
