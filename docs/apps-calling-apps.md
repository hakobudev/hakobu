# Apps calling apps

Apps of one project reach each other on the project's network, and the project's page draws them left to right: apps that call others before the apps they call.

## Referring to another app

A variable can refer to another app of the project, filled in when the app deploys:

- `VITE_API_URL=${{api.URL}}`: its public address, `https://api.example.com`, for a browser. `${{api.HOST}}` is the host alone.
- `API_URL=${{api.PRIVATE_URL}}`: its address inside the project (`http://app-12.hakobu:8080`), from a server: quicker and never through Cloudflare.

## One address for two apps

An app can serve a path of its address by another app of the project, like a proxy in front of both (app **Settings → Routes**). With `/api` → `api`, `shop.example.com/api/…` reaches the api and the rest the shop.

One address, so the browser needs no CORS and shares cookies. The api sees the whole path, `/api` included, and needs no address of its own.
