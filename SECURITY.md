# Security policy

## Supported versions

forgejo-operator is in prerelease: the latest release, `v0.1.0-rc.1`, is a
release candidate. Only the latest release is supported. Fixes are published
as a new release; older releases are not patched, so upgrade to the latest
one before reporting a problem that may already be fixed.

## Reporting a vulnerability

Report vulnerabilities privately, through GitHub's private vulnerability
reporting: open the **Security** tab of
[lictl/forgejo-operator](https://github.com/lictl/forgejo-operator) and
choose **Report a vulnerability**. Include the version, how to reproduce the
problem, and its impact.

Never post exploitable details or credentials in public issues, pull
requests or discussions. Do not assume a particular email address from Git
commit metadata.

For an affected deployment, reduce exposure and rotate any compromised
credentials according to your cluster's procedures.
