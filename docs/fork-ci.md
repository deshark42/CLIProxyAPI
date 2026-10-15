# Fork maintenance, CI and container images

## Branch maintenance

- `main` mirrors `upstream/main` exactly. Do not merge fork patches or release branches back into it.
- `ci/fork-image-publishing` contains only the fork CI, Compose, and maintenance-documentation patches.
- `feat/codex-raw-http-passthrough` contains only the Codex passthrough feature and its fixes.
- Each `release/<version>` branch integrates both patch branches on the selected upstream base. Test and tag this branch, not clean `main`.

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

# Choose a new, unused integration branch for this release.
git switch -c release/v1.2.3-deshark.1 main
git merge --no-ff ci/fork-image-publishing -m "Merge fork CI into release integration"
git merge --no-ff feat/codex-raw-http-passthrough -m "Merge Codex passthrough into release integration"
# Resolve conflicts, review the combined changes, and run tests and the server build.
git push origin HEAD
```

Before rebasing published patch branches, record their old remote tips. If their commit IDs change, publish each rebased branch using an explicit `--force-with-lease=refs/heads/<branch>:<recorded-old-sha>` check. Ordinary upstream updates to clean `main` are fast-forwards and do not require a force push. Do not discard unexpected remote changes to make a push succeed.

Do not merge `main` or an integrated release into a patch branch to refresh it; rebase that branch onto clean `main` instead. Review upstream changes and remove superseded patches deliberately. Rebase can require conflict resolution and does not replace integration tests.

Never move or overwrite a published version tag. It preserves the complete source and CI configuration for that release even after patch branches are rebased. Old integration branches may be retained or deleted after confirming their published tags preserve the intended commits. Creating or pushing an integration branch alone does not publish an image.

## Container publishing

The `docker-image` workflow publishes to `ghcr.io/<owner>/<repository>`, with the repository name converted to lowercase. For this fork the image is `ghcr.io/deshark42/cliproxyapi`.

The fork workflow runs on pushes of `v*` tags and supports manual dispatch where GitHub makes it available. Ordinary pushes to patch or release branches do not trigger image publishing. Publish by creating a new `v*` tag on the tested release integration commit, which contains this workflow. Clean `main` deliberately retains the upstream workflow definitions; manual dispatch and PR checks depend on the workflow present on the default or PR target branch. Do not rely on the fork manual-dispatch UI being available on clean `main`.

| Ref | Example image tag |
| --- | --- |
| Pushed `v1.2.3-deshark.1` tag | `v1.2.3-deshark.1` and `latest` |
| Manually dispatched integrated release branch, when available | Branch tag (does not update `latest`) |
| Any published commit | `sha-<full-commit-sha>` |

Only builds of `v*` tag refs update `latest`, including manually dispatched builds of those tags. Every matching tag is treated as a release, including suffixes such as `-deshark.1`; tag pushes are not restricted to commits on `main`, so select the intended release commit carefully. Manually dispatched branch builds never overwrite `latest`. ARM64 builds are disabled by default to conserve Actions resources, so new images support only `linux/amd64`. To restore dual-architecture builds without changing code, set the repository Actions variable `ENABLE_ARM64_BUILDS` to `true`; an unset variable or any other value leaves ARM64 disabled. The selected architecture list is captured once per workflow and reused for the build matrix and manifest validation. The native build paths are retained: `ubuntu-latest` for AMD64 and `ubuntu-24.04-arm` for ARM64. QEMU is not used, so compilation runs natively while preserving CGO/plugin support. Each architecture has its own build cache. Architecture jobs push content-addressed images without updating the public tags; after all enabled architectures succeed, a final job combines their digests, publishes all tags, and verifies the enabled platform list. Native ARM runner availability and queue time depend on GitHub Actions capacity and repository eligibility. The build uses the model catalogs committed in the repository rather than fetching a newer upstream catalog during CI.

Authentication uses the workflow's built-in `GITHUB_TOKEN` with `contents: read` and `packages: write`. No Docker Hub account, Docker Hub secrets, or separate publishing PAT is needed. Repository or organization policies must permit Actions and package publishing. If Actions is disabled for the fork, enable it on the Actions page first.

A new GHCR package is private by default, even for a public repository. After the first publish, open the package settings and change visibility to **Public** if anonymous pulls are required. If a package with the same name already exists, grant this repository Actions write access to that package.

Do not tag clean `main` for fork releases: a tag selects the workflow definitions in its own commit, so a clean-main tag would use upstream publishing workflows instead.

Only when a release is explicitly authorized, tag the tested integration branch (choose an unused version; do not move existing release tags):

```bash
git switch release/v1.2.3-deshark.1
git tag -a v1.2.3-deshark.1 -m "Fork release"
git push origin v1.2.3-deshark.1
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

- The integrated `pr-test-build` workflow is called by the fork image workflow for every image build. It runs the configuration, OpenAI API handler, and configuration-diff tests and builds the server. These checks do not depend on feature-branch code. Image publishing waits for these checks to pass. PRs targeting clean `main` use upstream PR checks instead. This does not claim to run the entire repository test suite.
- Disable `agents-md-guard`, `translator-path-guard` (file `pr-path-guard.yml`), and `auto-retarget-main-pr-to-dev` in the fork repository Actions settings. Clean `main` contains their original upstream definitions, so guards present only in the CI patch are insufficient. The integrated definitions retain their upstream-repository guards as an additional safeguard.
- The multi-platform binary `release` workflow is separate from image publishing. It is disabled in forks by default. To opt in, create the repository Actions variable `ENABLE_BINARY_RELEASES` with value `true` before pushing a release tag. Existing upstream behavior is preserved.

The PR-management workflows use `pull_request_target`, which loads the workflow from the target branch, not the feature branch. Keep the three workflows disabled at repository level when updating clean `main`; recheck their state if upstream adds or renames workflows. Disabling these PR-management workflows does not disable tag-triggered image publishing.

A published image has passed the focused regression tests, server build, and container build; it does not imply that every repository test has passed.
