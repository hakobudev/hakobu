# Updates

**Settings → Updates** shows when a new release is out; **Update** installs it. Over SSH:

```bash
sudo /opt/hakobu/hakobu update
```

Apps keep running; the panel is down for the minute it restarts. If the new version doesn't come up, the previous one is put back by itself, and **Roll back** in the same place (or `sudo /opt/hakobu/hakobu rollback`) goes back to it later. A rollback past an update that changed the panel's database also puts back the copy of the database made just before it (what changed in the panel since is lost; the newer database is kept beside it).

A panel installed before v0.24.0 can't update to it or later: their databases have nothing in common. It refuses to start, and the update puts the old version back by itself. Install anew.

## Signed releases

Releases are signed with an Ed25519 key that never leaves the release workflow: hakobu and install.sh install a release only if its signature over `hakobu <version>` and its checksums is by that key, so neither a tampered download nor an older release under a newer name gets in.

The panel can't update itself: it runs as `hakobu`, which can't write its own program, and only asks; a root service (`hakobu-update.path`, `hakobu-rollback.path`) then installs the latest signed release or puts back the one from before the last update, whatever the request said. Installing any other version takes `sudo /opt/hakobu/hakobu update --to vX.Y.Z`.
