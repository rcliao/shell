package config

import (
	"reflect"
	"testing"
)

type fakeSecretStore struct{ data map[string]string }

func (f fakeSecretStore) Get(k string) (string, error) {
	if v, ok := f.data[k]; ok {
		return v, nil
	}
	return "", errNotFound
}
func (f fakeSecretStore) Set(k, v string) error { f.data[k] = v; return nil }
func (f fakeSecretStore) List() ([]string, error) {
	out := []string{}
	for k := range f.data {
		out = append(out, k)
	}
	return out, nil
}
func (f fakeSecretStore) Remove(k string) error { delete(f.data, k); return nil }
func (f fakeSecretStore) Close() error          { return nil }

var errNotFound = &notFoundErr{}

type notFoundErr struct{}

func (*notFoundErr) Error() string { return "not found" }

func TestManagedNamesAndPassthrough(t *testing.T) {
	old := globalSecretStore
	t.Cleanup(func() { globalSecretStore = old })
	globalSecretStore = nil
	if got := ManagedSecretNames(); got != nil {
		t.Fatalf("no store → nil, got %v", got)
	}

	globalSecretStore = fakeSecretStore{data: map[string]string{"GEMINI_API_KEY": "g", "TELEGRAM_BOT_TOKEN": "t"}}
	names := ManagedSecretNames()
	if len(names) != 2 {
		t.Fatalf("ManagedSecretNames = %v", names)
	}

	t.Setenv("TAVILY_API_KEY", "from-env")
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	var c Config // Passthrough nil → defaults
	got := c.SecretPassthrough()
	want := map[string]string{"GEMINI_API_KEY": "g", "TAVILY_API_KEY": "from-env"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SecretPassthrough = %v, want %v (store first, env fallback, unset absent)", got, want)
	}

	c.Secrets.Passthrough = []string{"TELEGRAM_BOT_TOKEN"} // explicit list wins, even a bot token
	if got := c.SecretPassthrough(); got["TELEGRAM_BOT_TOKEN"] != "t" || len(got) != 1 {
		t.Fatalf("explicit passthrough = %v", got)
	}
	c.Secrets.Passthrough = []string{} // explicitly nothing
	if got := c.SecretPassthrough(); len(got) != 0 {
		t.Fatalf("empty passthrough must pass nothing, got %v", got)
	}
}
