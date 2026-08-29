package github

import (
	"os"
	"testing"
)

func TestParseRepository(t *testing.T) {
	for _, test := range []struct {
		input string
		want  Repository
	}{
		{input: "octo/repo", want: Repository{Owner: "octo", Name: "repo"}},
		{input: "octo/repo.git", want: Repository{Owner: "octo", Name: "repo"}},
	} {
		got, err := ParseRepository(test.input)
		if err != nil || got != test.want {
			t.Fatalf("ParseRepository(%q) = %#v, %v", test.input, got, err)
		}
	}
	for _, input := range []string{"", "octo", "octo/", "octo/.git", "octo/repo/extra"} {
		if _, err := ParseRepository(input); err == nil {
			t.Fatalf("ParseRepository(%q) unexpectedly succeeded", input)
		}
	}
}

func TestDiscoverRepositoryEnvironment(t *testing.T) {
	old, had := os.LookupEnv("GITHUB_REPOSITORY")
	if err := os.Setenv("GITHUB_REPOSITORY", "octo/repo"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("GITHUB_REPOSITORY", old)
		} else {
			_ = os.Unsetenv("GITHUB_REPOSITORY")
		}
	})
	got, err := DiscoverRepository()
	if err != nil || got.String() != "octo/repo" {
		t.Fatalf("DiscoverRepository() = %#v, %v", got, err)
	}
}
