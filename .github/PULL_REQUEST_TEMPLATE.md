## Description

<!-- Explain the problem and the user-visible outcome. -->
<!-- Link issues with "Closes #123". -->

## Release intent

<!-- Keep the fences exactly as they are; CI parses between them. -->
<!-- Set each bump to none, patch, minor, or major. Leave the key names. -->
<!-- `services` must be >= the highest component bump. -->
<!-- Set the title and body to n/a when no bump is a release. -->
<!-- The release version users see is NOT declared here: it patch-bumps by
     itself whenever any bump above is a release. -->
<!-- Keep the changelog body to prose and bullets. A line starting with "### "
     ends the section and silently truncates the rest. -->

<!-- pair-release-intent:v1 -->
### Changelog title
n/a

### Changelog body
n/a

### Bumps
- services: none
- nvpair-cluster-manager: none
- nvpair-engine-manager: none
- nvpair-errors: none
- nvpair-job-scheduler: none
- nvpair-manual-nodes: none
- nvpair-node-info: none
- nvpair-node-scanner: none
- nvpair-node-settings: none
- nvpair-proxy: none
- nvpair-tui: none
- nvpair-ui-broker: none
- nvpair-workload-manager: none
<!-- /pair-release-intent:v1 -->

## Scope

<!-- What is intentionally included and excluded? -->

## Validation

<!-- List exact commands, environments, and manual checks. -->

## Risk

<!-- Note compatibility, security, data, packaging, or migration concerns. -->

## Checklist

- [ ] I have read the [Contributing Guidelines](https://github.com/NVIDIA/Personal-AI-Router/blob/main/CONTRIBUTING.md).
- [ ] Every commit is signed off (`git commit -s`), certifying the [Developer Certificate of Origin](https://developercertificate.org/).
- [ ] New or existing tests cover the change.
- [ ] Relevant documentation is updated.
- [ ] I checked the diff, changed filenames, and commit messages for credentials, private data, internal URLs, internal issue identifiers, and generated artifacts.
- [ ] I recorded the validation commands and results above.
- [ ] I declared version bumps in the release-intent block above. `services/versions.json` is written by automation — do not edit it by hand.
