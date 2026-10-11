# Volumes

A volume is a directory that survives redeploys, shared by the app and its worker: for an SQLite database, uploads or anything else on disk. Everything else in the container is reset on every deploy.

Add one in the app's **Settings → Volumes** with its mount path, `/app/data` say.

## Deploys

An app with volumes is stopped before its new version starts, so two versions never write the same files: a few seconds of downtime. If your app can share them safely, let both versions share the volumes and keep deploys without downtime.

## Backups

With backups on, each volume is backed up daily to the backup bucket, like a database. The app and its worker are paused for the seconds tar takes to read it, so an SQLite file and its journal are copied as of one moment. Each backup is sealed with the server's key, read back after upload, and can't be deleted in its first 7 days. One click on the app's page puts a backup back.

## Uploading a file

An SQLite database (or any file) goes into a volume from the panel: app **Settings → Volumes → Upload a file**, with its path in the volume, e.g. `app.db`. The volume is backed up first, then the app stops while the file is written in place of one of that name, owned as the volume is. It goes to the app's server through the backup bucket, sealed, so backups must be on. Cloudflare passes up to 100 MB in one upload.
