package pipeline

import (
	"testing"

	"github.com/seedhire/mantis/internal/router"
)

func TestShouldRun(t *testing.T) {
	codeIntent := router.Intent{Tier: router.TierCode, TaskType: "implement"}
	reasonIntent := router.Intent{Tier: router.TierReason, TaskType: "design"}

	should := []string{
		"build a web app",
		"create a REST API with database",
		"implement a todo app from scratch",
		"build a CLI tool with auth and config",
		"create a full stack application",
		"develop a microservice",
		"write a backend with JWT auth and db schema",
		"build a web server with routes and middleware",
	}
	shouldNot := []string{
		"fix this bug in parseUser",
		"explain how defer works",
		"refactor this function",
		"what is the difference between sync.Mutex and sync.RWMutex",
		"write a unit test for fetchUser",
		"rename this variable",
	}

	for _, msg := range should {
		if !ShouldRun(codeIntent, msg) {
			t.Errorf("expected pipeline for %q, got false", msg)
		}
		if !ShouldRun(reasonIntent, msg) {
			t.Errorf("expected pipeline (reason) for %q, got false", msg)
		}
	}
	for _, msg := range shouldNot {
		if ShouldRun(codeIntent, msg) {
			t.Errorf("expected NO pipeline for %q, got true", msg)
		}
	}
}

func TestShouldRunNeverForBlockedTiers(t *testing.T) {
	msg := "build a web app with database and auth"
	blocked := []router.Tier{router.TierMax, router.TierTrivial, router.TierFast, router.TierVision}
	for _, tier := range blocked {
		intent := router.Intent{Tier: tier}
		if ShouldRun(intent, msg) {
			t.Errorf("pipeline should never run for tier %s, but got true", tier)
		}
	}
}
