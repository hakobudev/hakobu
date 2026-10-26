# Alerts and audit

The server watches itself and emails you through any SMTP server (`[alerts.smtp]` in its [config](servers.md)): one email when a problem starts and one when it's over, never a stream. Without SMTP, problems show only in `hakobu status`.

## What it watches

- an app that hasn't answered its health check for 3 minutes;
- a failed deploy (closed by the next one that works) or backup, and a backup that didn't prove it restores;
- the disk 85% full (closed under 80%);
- PostgreSQL not answering;
- an update of hakobu that didn't install (closed once it does), and a newer minor release of hakobu out for 14 days that this server hasn't updated to;
- the **audit**, hourly:
  - a port listening beyond loopback and the tailnet that isn't in `alerts.allow_ports` (80 and 443 are allowed for `public` ingress by themselves; 22 with the API on loopback, as SSH is the way to it; else add 22 if SSH listens on every address);
  - a container hakobu didn't start;
  - unattended upgrades that haven't run for 3 days.

What's fixed closes by itself. Problems survive a restart of hakobu, so you aren't mailed twice.

The audit sees what listens on the machine, not a firewall in front of it (a cloud firewall is outside its view), and only TCP.

## The pulse

Every 15 minutes hakobu fetches `alerts.pulse_url`, or `alerts.pulse_fail_url` while a problem is open. Point them at a dead man's switch ([healthchecks.io](https://healthchecks.io) and the like): it emails you when the pulses stop, which is how you hear that hakobu, or the whole server, is down.

## Your apps' errors

hakobu doesn't collect errors or traces. Give an app a `SENTRY_DSN` [secret](secrets.md) and any Sentry-compatible service has them.
