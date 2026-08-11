package childenv

import (
	"reflect"
	"testing"
)

func TestFilterRemovesSensitiveNames(t *testing.T) {
	t.Parallel()

	input := []string{
		"PATH=test-path",
		"ROVER_SECRET=test-only",
		"ROVER_PROXY_VERIFY=untrusted-parent-value",
		"SERVICE_SECRET=test-only",
		"service_token=test-only",
		"SIGNING_KEY=test-only",
		"API_KEYSTONE=kept",
		"TOKEN_BUCKET=kept",
		"NORMAL=a=b",
	}
	want := []string{
		"PATH=test-path",
		"API_KEYSTONE=kept",
		"TOKEN_BUCKET=kept",
		"NORMAL=a=b",
	}

	if got := Filter(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("Filter() = %q; want %q", got, want)
	}
}
