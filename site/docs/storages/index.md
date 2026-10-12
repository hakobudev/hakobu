# Storages

A storage is a bucket for your apps' files: R2 in the project's Cloudflare account, or any S3-compatible service (AWS, Backblaze, MinIO, …).

## R2

**New → Storage** with R2: hakobu creates the bucket in the project's Cloudflare account, or, under **Bucket**, takes one already there with its files (an app moving in keeps them; the backups' bucket isn't offered). The keys the app gets come from an R2 API token you make for that bucket alone, as the storage's page shows step by step:

1. In Cloudflare, open **R2 → API tokens** and create an **Account API token**.
2. Permissions: **Object Read & Write**; buckets: **Apply to specific buckets only**, with the storage's bucket.
3. Paste its Access Key ID and Secret Access Key on the storage's page.

hakobu checks the keys reach their bucket and not the backups, before it saves them. (Its own token can't make such keys, and its keys would reach every bucket, the backups included.) R2's free plan includes 10 GB, and downloads are free.

## S3-compatible

Give the endpoint, bucket, keys and region (default `auto`) when you create the storage.

## Linking

Link a storage in the app's **Settings → Resources**. The app gets `S3_ENDPOINT`, `S3_BUCKET_NAME`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` and `S3_REGION` on its next deploy.

Removing a storage only makes hakobu forget it: the bucket and its files stay.
