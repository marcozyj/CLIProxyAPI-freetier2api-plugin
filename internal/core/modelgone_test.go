package core

import (
	"errors"
	"fmt"
	"testing"
)

func TestModelUnavailableMatchesOnlyMissingModelErrors(t *testing.T) {
	gone := []string{
		`upstream client [400]: {"message":"非法模型"}`,
		"model not found: glm-9",
		"model_not_found",
		"invalid model: foo",
		"unknown model bar",
		"no such model baz",
		"the model does not exist",
		"模型不存在",
	}
	for _, message := range gone {
		if !ModelUnavailable(errors.New(message)) {
			t.Fatalf("ModelUnavailable(%q) = false, want true", message)
		}
	}

	keep := []string{
		"invalid api key",
		"rate limited",
		"model does not support tools",
		"model is required",
		"insufficient quota",
		"request timeout",
	}
	for _, message := range keep {
		if ModelUnavailable(errors.New(message)) {
			t.Fatalf("ModelUnavailable(%q) = true, want false", message)
		}
	}
	if ModelUnavailable(nil) {
		t.Fatal("nil error must not be classified as model-gone")
	}

	wrapped := fmt.Errorf("execute: %w", errors.New("model not found"))
	if !ModelUnavailable(wrapped) {
		t.Fatal("wrapped errors must still be classified")
	}
}
