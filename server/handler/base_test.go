// Copyright 2026 G-Research Limited
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-github/v53/github"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/G-Research/tfe-plan-bot/plan"
	"github.com/G-Research/tfe-plan-bot/pull"
)

// fakePullContext implements pull.Context by embedding the interface itself
// and overriding only the methods PostStatus actually calls. Any other
// method being invoked would panic on the nil embedded interface, which is
// intentional: it would mean PostStatus started depending on something this
// test doesn't know to expect.
type fakePullContext struct {
	pull.Context

	owner string
	repo  string
	sha   string

	statuses    map[string]*github.RepoStatus
	statusesErr error
}

func (f *fakePullContext) RepositoryOwner() string { return f.owner }
func (f *fakePullContext) RepositoryName() string  { return f.repo }
func (f *fakePullContext) HeadSHA() string         { return f.sha }

func (f *fakePullContext) LatestDetailedStatuses() (map[string]*github.RepoStatus, error) {
	return f.statuses, f.statusesErr
}

// newTestGithubClient returns a github.Client pointed at a local test
// server, and a *bool that flips to true if that server ever receives a
// request. This lets tests assert "was CreateStatus called at all" without
// caring about the exact request shape.
func newTestGithubClient(t *testing.T) (client *github.Client, called *bool, closeFn func()) {
	t.Helper()

	wasCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		wasCalled = true
		assert.Equal(t, http.MethodPost,
