# Fork maintenance, CI and container images

## Branch maintenance

- `main` mirrors `upstream/main` exactly. Do not merge fork patches or release branches back into it.
- `ci/fork-image-publishing` contains only the fork CI, Compose, and maintenance-documentation patches.
- `feat/codex-raw-http-passthrough` contains only the Codex passthrough feature and its fixes.
- Releases are published from a throwaway local integration commit: merge both patch branches on the selected upstream base, test it, tag it, and push only the tag. No release branch is kept on the remote; the published tag preserves the integrated tree.

This guide belongs to the CI patch and integrated releases; it is intentionally absent from clean `main`.

For each upstream update, start with a clean working tree:

```bash
git fetch --no-tags upstream
git fetch --no-tags origin
git switch main
git merge --ff-only upstream/main
git push origin main

# Reapply each independent patch series to the new upstream base.
git rebase main ci/fork-image-publishing
git rebase main feat/codex-raw-http-passthrough

# Create a throwaway local integration branch for this release.
git switch -c release/v1.2.3-deshark.1 main
git merge --no-ff ci/fork-image-publishing -m "Merge fork CI into release integration"
git merge --no-ff feat/codex-raw-http-passthrough -m "Merge Codex passthrough into release integration"
# Resolve conflicts, review the combined changes, and run the tests and server build locally before tagging.
# Do not push this branch: releases are published as tags only (see below).
```

Before rebasing published patch branches, record their old remote tips. If their commit IDs change, publish each rebased branch using an explicit `--force-with-lease=refs/heads/<branch>:<recorded-old-sha>` check. Ordinary upstream updates to clean `main` are fast-forwards and do not require a force push. Do not discard unexpected remote changes to make a push succeed.

Do not merge `main` or an integrated release into a patch branch to refresh it; rebase that branch onto clean `main` instead. Review upstream changes and remove superseded patches deliberately. Rebase can require conflict resolution and does not replace integration tests.

Never move or overwrite a published version tag. It preserves the complete source and CI configuration for that release even after patch branches are rebased, and it is the only reference a release needs: integration branches are local and are deleted after tagging. Creating an integration branch, with or without pushing it, does not publish an image.

## Container publishing

The `docker-image` workflow publishes to `ghcr.io/<owner>/<repository>`, with the repository name converted to lowercase. For this fork the image is `ghcr.io/deshark42/cliproxyapi`.

The fork workflow runs on pushes of `v*` tags and supports manual dispatch where GitHub makes it available. Ordinary pushes to patch or release branches do not trigger image publishing. Publish by creating a new `v*` tag on the tested integration commit, which contains this workflow, and pushing only that tag. Clean `main` deliberately retains the upstream workflow definitions; manual dispatch and PR checks depend on the workflow present on the default or PR target branch. Do not rely on the fork manual-dispatch UI being available on clean `main`.

| Ref | Example image tag |
| --- | --- |
| Pushed `v1.2.3-deshark.1` tag | `v1.2.3-deshark.1` and `latest` |
| Manually dispatched build of an integrated commit, when available | Branch tag (does not update `latest`) |
| Any published commit | `sha-<full-commit-sha>` |

Only builds of `v*` tag refs update `latest`, including manually dispatched builds of those tags. Every matching tag is treated as a release, including suffixes such as `-deshark.1`; tag pushes are not restricted to commits on `main`, so select the intended release commit carefully. Manually dispatched branch builds never overwrite `latest`. ARM64 builds are disabled by default to conserve Actions resources, so new images support only `linux/amd64`. To restore dual-architecture builds without changing code, set the repository Actions variable `ENABLE_ARM64_BUILDS` to `true`; an unset variable or any other value leaves ARM64 disabled. The selected architecture list is captured once per workflow and reused for the build matrix and manifest validation. The native build paths are retained: `ubuntu-latest` for AMD64 and `ubuntu-24.04-arm` for ARM64. QEMU is not used, so compilation runs natively while preserving CGO/plugin support. Each architecture has its own build cache. Architecture jobs push content-addressed images without updating the public tags; after all enabled architectures succeed, a final job combines their digests, publishes all tags, and verifies the enabled platform list. Native ARM runner availability and queue time depend on GitHub Actions capacity and repository eligibility. The build uses the model catalogs committed in the repository rather than fetching a newer upstream catalog during CI. Architecture jobs never create per-architecture tags either, so there is no temporary-tag cleanup step: upstream pushed `:<version>-amd64` tags to Docker Hub and deleted them afterwards, while this workflow combines the pushed digests directly. A cancelled or failed run can therefore only leave untagged digests behind, never a public tag.

Authentication uses the workflow's built-in `GITHUB_TOKEN` with `contents: read` and `packages: write`. No Docker Hub account, Docker Hub secrets, or separate publishing PAT is needed. Repository or organization policies must permit Actions and package publishing. If Actions is disabled for the fork, enable it on the Actions page first.

A new GHCR package is private by default, even for a public repository. After the first publish, open the package settings and change visibility to **Public** if anonymous pulls are required. If a package with the same name already exists, grant this repository Actions write access to that package.

Do not tag clean `main` for fork releases: a tag selects the workflow definitions in its own commit, so a clean-main tag would use upstream publishing workflows instead.

Only when a release is explicitly authorized, tag the tested integration commit and push only that tag (choose an unused version; do not move existing release tags):

```bash
git tag -a v1.2.3-deshark.1 -m "Fork release"
git push origin refs/tags/v1.2.3-deshark.1
git branch -D release/v1.2.3-deshark.1
```

Once the tag-triggered image workflow has succeeded:

```bash
docker pull ghcr.io/deshark42/cliproxyapi:latest

# Use the existing Compose configuration without rebuilding locally.
export CLI_PROXY_IMAGE=ghcr.io/deshark42/cliproxyapi:latest
docker compose pull
docker compose up -d --no-build
```

The Compose default already uses `ghcr.io/deshark42/cliproxyapi:latest`; the environment override above is optional. To pin a deployment to a particular source commit, use its `sha-<full-commit-sha>` tag (or the image digest).

## Remaining workflows

- Image publishing runs no tests: the fork image workflow builds and publishes exactly the tagged commit, so run the tests and server build locally before creating a release tag. The upstream `pr-test-build` workflow is left unmodified and only runs for pull requests, so PRs targeting clean `main` use the upstream definitions.
- Disable `agents-md-guard`, `translator-path-guard` (file `pr-path-guard.yml`), and `auto-retarget-main-pr-to-dev` in the fork repository Actions settings. Clean `main` contains their original upstream definitions, so guards present only in the CI patch are insufficient. The integrated definitions also pin the job condition to `false`, so they stay inert from this branch even if the repository-level disable is lifted.
- The multi-platform binary `release` workflow is separate from image publishing. The integrated definitions disable it with `if: false`. To publish binaries from the fork, replace that condition, for example with `${{ vars.ENABLE_BINARY_RELEASES == 'true' }}` and set the repository variable. Existing upstream behavior is preserved.

The PR-management workflows use `pull_request_target`, which loads the workflow from the target branch, not the feature branch. Keep the three workflows disabled at repository level when updating clean `main`; recheck their state if upstream adds or renames workflows. Disabling these PR-management workflows does not disable tag-triggered image publishing.

A published image has passed the container build only: publishing is not gated on any test job, so the image does not imply that any test suite passed.
