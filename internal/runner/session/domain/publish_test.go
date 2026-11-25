package domain

import (
	"errors"
	"testing"
)

func publish() Assignment {
	return Assignment{Step: "STEP1", Fence: 1, Execution: "EX1", Task: "T1", Project: "P1", Repository: "acme/app",
		Adapter: AdapterPublish, Role: RolePublisher, Base: "dev", DiffHead: "0123456789abcdef0123456789abcdef01234567"}
}

// KIN-36: a publish assignment pushes the reviewed commit; it carries no agent fields and needs a base and a
// full commit id.
func TestCheckPublishAssignment(t *testing.T) {
	t.Parallel()
	if err := CheckAssignment(publish(), Policy{}); err != nil {
		t.Fatal(err)
	}
	if !publish().Publish() || valid().Publish() {
		t.Fatal("Publish")
	}
	for name, mutate := range map[string]func(a *Assignment){
		"no head":      func(a *Assignment) { a.DiffHead = "" },
		"short head":   func(a *Assignment) { a.DiffHead = "0123abc" },
		"revision":     func(a *Assignment) { a.DiffHead = "HEAD" },
		"no base":      func(a *Assignment) { a.Base = "" },
		"prompt":       func(a *Assignment) { a.Prompt = "push it" },
		"scenario":     func(a *Assignment) { a.Scenario = "success" },
		"test command": func(a *Assignment) { a.TestCommand = "make" },
		"other role":   func(a *Assignment) { a.Role = RoleReviewer },
	} {
		a := publish()
		mutate(&a)
		if err := CheckAssignment(a, Policy{}); !errors.Is(err, ErrInvalidAssignment) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
