package deploy

import (
	"context"
	"encoding/json"
	"net/http"
)

const NetworkName = "hako"

func EnsureNetwork(ctx context.Context) error {
	return EnsureNamedNetwork(ctx, NetworkName)
}

// EnsureNamedNetwork creates a bridge network with the given name if it
// doesn't already exist.
func EnsureNamedNetwork(ctx context.Context, name string) error {
	_, status, err := dockerRequest(ctx, "GET", "/networks/"+name, nil)
	if err == nil && status == http.StatusOK {
		return nil // мережа вже існує
	}

	createBody := map[string]interface{}{
		"Name":   name,
		"Driver": "bridge",
	}
	respBody, status, err := dockerRequest(ctx, "POST", "/networks/create", createBody)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return errFromBody(respBody, status)
	}
	return nil
}

func errFromBody(body []byte, status int) error {
	var e struct {
		Message string `json:"message"`
	}
	json.Unmarshal(body, &e)
	return &dockerError{status: status, message: e.Message}
}

type dockerError struct {
	status  int
	message string
}

func (e *dockerError) Error() string {
	return e.message
}
