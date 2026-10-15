# Fork CI and container images

## Container publishing

The `docker-image` workflow publishes to `ghcr.io/<owner>/<repository>`, with the repository name converted to lowercase. For this fork the image is `ghcr.io/deshark42/cliproxyapi`.

It runs on pushes of `v*` tags and manual dispatch. Ordinary pushes to `main` or feature branches only update code and do not trigger image publishing; pull requests still run the build checks. After merging the intended changes into `main`, create and push a new `v*` tag to publish a release image. GitHub only exposes the manual workflow in the Actions UI after the workflow exists on the default branch.

| Ref | Example image tag |
| --- | --- |
| Pushed `v1.2.3-deshark.1` tag | `v1.2.3-deshark.1` and `latest` |
| Manually dispatched branch, e.g. `main` | `main` (does not update `latest`) |
| Any published commit | `sha-<full-commit-sha>` |

Only builds of `v*` tag refs update `latest`, including manually dispatched builds of those tags. Every matching tag is treated as a release, including suffixes such as `-deshark.1`; tag pushes are not restricted to commits on `main`, so select the intended release commit carefully. Manually dispatched branch builds never overwrite `latest`. ARM64 builds are disabled by default to conserve Actions resources, so new images support only `linux/amd64`. To restore dual-architecture builds without changing code, set the repository Actions variable `ENABLE_ARM64_BUILDS` to `true`; an unset variable or any other value leaves ARM64 disabled. The selected architecture list is captured once per workflow and reused for the build matrix and manifest validation. The native build paths are retained: `ubuntu-latest` for AMD64 and `ubuntu-24.04-arm` for ARM64. QEMU is not used, so compilation runs natively while preserving CGO/plugin support. Each architecture has its own build cache. Architecture jobs push content-addressed images without updating the public tags; after all enabled architectures succeed, a final job combines their digests, publishes all tags, and verifies the enabled platform list. Native ARM runner availability and queue time depend on GitHub Actions capacity and repository eligibility. The build uses the model catalogs committed in the repository rather than fetching a newer upstream catalog during CI.

Authentication uses the workflow's built-in `GITHUB_TOKEN` with `contents: read` and `packages: write`. No Docker Hub account, Docker Hub secrets, or separate publishing PAT is needed. Repository or organization policies must permit Actions and package publishing. If Actions is disabled for the fork, enable it on the Actions page first.

A new GHCR package is private by default, even for a public repository. After the first publish, open the package settings and change visibility to **Public** if anonymous pulls are required. If a package with the same name already exists, grant this repository Actions write access to that package.

For example, after the intended changes have reached `main`, publish a new release tag (choose an unused version; do not move existing release tags):

```bash
git switch main
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

- `pr-test-build` remains enabled for pull requests and manual dispatch, and is also called by the image workflow for every image build (tag pushes and manual dispatch). It runs the existing configuration, OpenAI API handler, and configuration-diff tests and builds the server. These checks do not depend on feature-branch code. Image publishing waits for these checks to pass. This does not claim to run the entire repository test suite.
- `agents-md-guard`, `translator-path-guard`, and `auto-retarget-main-pr-to-dev` run only in `router-for-me/CLIProxyAPI`. In this fork they skip their jobs and do not close PRs, restrict translator changes, or retarget PRs to an upstream `dev` branch.
- The multi-platform binary `release` workflow is separate from image publishing. It is disabled in forks by default. To opt in, create the repository Actions variable `ENABLE_BINARY_RELEASES` with value `true` before pushing a release tag. Existing upstream behavior is preserved.

The PR-management workflows use `pull_request_target`, which loads the workflow from the target branch, not the feature branch. Their new fork guards take effect only after these changes reach that branch. If opening the first PR before then, temporarily disable the old PR-management workflows in the fork's Actions settings to avoid the old auto-retarget behavior. This does not affect publishing images from version-tag pushes.

A published image has passed the focused regression tests, server build, and container build; it does not imply that every repository test has passed.
