package main

import (
	"context"
	"os"

	"dagger.io/dagger"
)

// currentWorkspace supports the helper's legacy gateway mode. Module calls use
// the local snapshot mode and do not depend on an ambient workspace.
func currentWorkspace(ctx context.Context) (*dagger.Workspace, error) {
	client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return nil, err
	}
	return client.CurrentWorkspace(), nil
}
