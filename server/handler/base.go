// Copyright 2018 Palantir Technologies, Inc.
// Copyright 2020 G-Research Limited
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
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-github/v53/github"
	"github.com/palantir/go-baseapp/baseapp"
	"github.com/palantir/go-githubapp/githubapp"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/palantir/policy-bot/policy/common"
	"github.com/palantir/policy-bot/policy/reviewer"
	pbpull "github.com/palantir/policy-bot/pull"

	"github.com/G-Research/tfe-plan-bot/plan"
	"github.com/G-Research/tfe-plan-bot/pull"
)

const (
	DefaultPolicyPath         = ".tfe-plan.yml"
	DefaultStatusCheckContext = "TFE"

	LogKeyGitHubSHA = "github_sha"
)

type Base struct {
	githubapp.ClientCreator

	Installations     githubapp.InstallationsService
	GlobalCache       pbpull.GlobalCache
	ConfigFetcher     *ConfigFetcher
	BaseConfig        *baseapp.HTTPConfig
	TFEClientProvider *plan.ClientProvider
	HTTPClient        *http.Client
	PullOpts          *PullEvaluationOptions

	AppName string
}

type PullEvaluationOptions struct {
	ConfigPath string `yaml:"config_path"`

	// StatusCheckContext will be used to create the status context. It will be used in the following
	// pattern: <StatusCheckContext>/<TFE Organization Name>/<TFE Workspace Name>
	StatusCheckContext string `yaml:"status_check_context"`

	// This field is unused but is left to avoid breaking configuration files:
	// yaml.UnmarshalStrict returns an error for unmapped fields
	//
	// TODO(jgiannuzzi): remove in version 1.0
	Deprecated_AppName string `yaml:"app_name"`
}

func (p *PullEvaluationOptions) FillDefaults() {
	if p.ConfigPath == "" {
		p.ConfigPath = DefaultPolicyPath
	}

	if p.StatusCheckContext == "" {
		p.StatusCheckContext = DefaultStatusCheckContext
	}
}

func (b *Base) PostStatus(ctx context.Context, prctx pull.Context, wkcfg plan.WorkspaceConfig, runID string, client *github.Client, state, message string) error {
	owner := prctx.RepositoryOwner()
	repo := prctx.RepositoryName()
	sha := prctx.HeadSHA()
	statusContext := b.statusCheckContext(wkcfg)

	var targetURL string
	if runID != "" {
		targetURL = b.targetURL(wkcfg, runID)
	}

	// Skip the write entirely if the status we are about to post is
	// identical to what is already on the commit. The bot can be
	// re-evaluated many times for the same, unchanged commit - a burst of
	// unrelated commits/reviews/checks on the same PR each cause a full
	// workspace sweep - and most of those re-evaluations produce exactly
	// the same result as before. Without this check, every one of them
	// still pays for a fresh POST /repos/:owner/:repo/statuses/:sha, which
	// is by far the most expensive call this bot makes (roughly 1s each,
	// since writing a status invalidates the commit's combined-status
	// rollup and fans out webhooks to every subscriber - including this
	// bot itself). This check costs nothing extra: LatestDetailedStatuses
	// is already fetched and cached once per evaluation round.
	existing, err := prctx.LatestDetailedStatuses()
	if err != nil {
		return errors.Wrap(err, "failed to check existing status before posting")
	}
	if current, ok := existing[statusContext]; ok {
		if current.GetState() == state && current.GetDescription() == message && current.GetTargetURL() == targetURL {
			zerolog.Ctx(ctx).Debug().Msgf("Status %q on %s is already up to date, skipping write", statusContext, sha)
			return nil
		}
	}

	status := &github.RepoStatus{
		Context:     github.String(statusContext),
		State:       &state,
		Description: &message,
	}
	if targetURL != "" {
		status.TargetURL = github.String(targetURL)
	}

	return b.postGitHubRepoStatus(ctx, client, owner, repo, sha, status)
}

func (b *Base) postGitHubRepoStatus(ctx context.Context, client *github.Client, owner, repo, ref string, status *github.RepoStatus) error {
	logger := zerolog.Ctx(ctx)
	logger.Info().Msgf("Setting %q status on %s to %s: %s", status.GetContext(), ref, status.GetState(), status.GetDescription())
	_, _, err := client.Repositories.CreateStatus(ctx, owner, repo, ref, status)
	return err
}

func (b *Base) statusCheckContext(wkcfg plan.WorkspaceConfig) string {
	return fmt.Sprintf("%s/%s", b.PullOpts.StatusCheckContext, wkcfg)
}

func (b *Base) targetURL(wkcfg plan.WorkspaceConfig, runID string) string {
	return fmt.Sprintf("%s/app/%s/workspaces/%s/runs/%s", b.TFEClientProvider.Address(), wkcfg.Organization, wkcfg.Name, runID)
}

func (b *Base) PreparePRContext(ctx context.Context, installationID int64, pr *github.PullRequest) (context.Context, zerolog.Logger) {
	ctx, logger := githubapp.PreparePRContext(ctx, installationID, pr.GetBase().GetRepo(), pr.GetNumber())

	logger = logger.With().Str(LogKeyGitHubSHA, pr.GetHead().GetSHA()).Logger()
	ctx = logger.WithContext(ctx)

	return ctx, logger
}

func (b *Base) Evaluate(ctx context.Context, installationID int64, trigger common.Trigger, loc pull.Locator) error {
	client, err := b.NewInstallationClient(installationID)
	if err != nil {
		return err
	}

	v4client, err := b.NewInstallationV4Client(installationID)
	if err != nil {
		return err
	}

	mbrCtx := NewCrossOrgMembershipContext(ctx, client, loc.Owner, b.Installations, b.ClientCreator)
	prctx, err := pull.NewGitHubContext(ctx, mbrCtx, b.GlobalCache, client, v4client, b.HTTPClient, loc)
	if err != nil {
		return err
	}

	fetchedConfig, err := b.ConfigFetcher.ConfigForPR(ctx, prctx, client)
	if err != nil {
		return errors.WithMessage(err, fmt.Sprintf("failed to fetch policy: %s", fetchedConfig))
	}

	return b.EvaluateFetchedConfig(ctx, prctx, client, fetchedConfig, trigger)
}

func (b *Base) EvaluateFetchedConfig(ctx context.Context, prctx pull.Context, client *github.Client, fetchedConfig FetchedConfig, trigger common.Trigger) error {
	logger := zerolog.Ctx(ctx)

	if fetchedConfig.Missing() {
		logger.Debug().Msgf("Policy does not exist: %s", fetchedConfig)
		return nil
	}

	if fetchedConfig.Invalid() {
		logger.Warn().Err(fetchedConfig.Error).Msgf("Invalid policy: %s", fetchedConfig)
		return nil
	}

	// Pre-fetch changed files once before fanning out to workspace goroutines.
	// The pull.Context is not thread-safe, so this ensures a single API call
	// to GET /repos/:owner/:repo/pulls/:number/files and populates the cache
	// before concurrent access.
	_, err := prctx.ChangedFiles()
	if err != nil {
		return errors.Wrap(err, "failed to list pull request files")
	}

	var wg sync.WaitGroup
	var evaluationFailures uint32
	for i := range fetchedConfig.Config.Workspaces {
		wkcfg := fetchedConfig.Config.Workspaces[i]

		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := b.EvaluateWorkspace(ctx, prctx, client, fetchedConfig, trigger, wkcfg); err != nil {
				atomic.AddUint32(&evaluationFailures, 1)
				logger.Error().Err(err).Msgf("Failed to evaluate workspace %s", wkcfg)
			}
		}()
	}

	wg.Wait()

	if
