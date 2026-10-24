# Workshop network settings

[简体中文（默认）](workshop-network.md) | **English**

Open **System settings → Workshop** (`/system/settings?tab=workshop`) to configure
how the management service retrieves mod names, search results, details and images.
Failed searches also provide a **Network settings** shortcut. Saving applies changes
immediately while worlds run, without restarting the service or disconnecting Agents.

## Connection modes

| Mode | Behavior |
| --- | --- |
| System proxy (default) | Follow the management process's `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` |
| Direct | Ignore system proxy settings and connect directly |
| Custom proxy | Use an HTTP, HTTPS or `socks5h` proxy for Workshop metadata only |

Examples: `http://proxy.example.com:7890` or `socks5h://proxy.example.com:1080`.
With `socks5h`, the proxy resolves destination names. Proxy URLs support credentials;
URL-encode special characters. The saved proxy URL is treated as a secret and is never
returned in plaintext. Leave it blank to retain it; select Direct or System proxy to stop using it.

Inside Docker, `127.0.0.1` / `localhost` refers to the container. Use a host address
reachable from the management container, and allow connections from that container.

## Optional API key and reverse proxies

Obtain a Steam Web API key from [Steam](https://steamcommunity.com/dev/apikey).
Saved keys are never displayed in plaintext. Leave the field blank to keep the key,
or click **Clear** and save to remove it.

| Available connection | Name search | Mod ID / Workshop link lookup |
| --- | --- | --- |
| Key configured, Steam API reachable | Steam API | Steam API |
| No key, Steam Community reachable | Public Workshop pages | Keyless API |
| No key, Community unreachable but API reachable | Unavailable | Still available |

A key does not fix every network issue. The API, Community and image CDN are separate
destinations: API availability does not guarantee images or Community metadata.

Under **Advanced settings**, configure separate API and Community base URLs for an
administrator-maintained reverse proxy. Path prefixes are supported, for example
`https://proxy.example.com/steam-api`. Enter the service base URL without an API method,
key, query string or credentials. Defaults use official HTTPS endpoints with certificate
validation. An explicitly configured HTTP API URL may transmit the key without encryption.
The key is sent only to the configured API endpoint.

## Testing and searching

**Test connection** tests the current form without saving it. It checks name search,
ID lookup and images separately and reports elapsed time. If search is unavailable,
enter a known mod ID under Advanced settings to test the other stages independently.
A missing sample is shown as **Not tested**. Click **Save** to apply the form afterward.

Search accepts names, numeric mod IDs and standard Steam Workshop detail links. Links
only provide an ID; requests still use configured endpoints. Failed searches retain the
previous results. Cached responses are labeled with their collection time. **Try public
Workshop** starts a new search from page one. Pagination keeps its source; search again
after changing network settings.

Authors and Community descriptions load separately from the main search. Images use
authenticated same-origin thumbnail requests, with placeholders on failure. Network
failures preserve known metadata. Historical display data never proves that an installed
mod is current. Wait before retrying when the upstream service applies a rate limit.

## Configuration file and environment

The management service also reads these settings from `[mod]` in `conf/app.conf`:

| Configuration key | Environment variable | Default |
| --- | --- | --- |
| `STEAM_WEB_API_KEY` | `DST_ADMIN_STEAM_API_KEY` | Empty |
| `STEAM_API_BASE` | `DST_ADMIN_STEAM_API_BASE` | `https://api.steampowered.com` |
| `STEAM_COMMUNITY_BASE` | `DST_ADMIN_STEAM_COMMUNITY_BASE` | `https://steamcommunity.com` |
| `STEAM_PROXY_MODE` | `DST_ADMIN_STEAM_PROXY_MODE` | `system`; also accepts `direct` / `custom` |
| `STEAM_HTTP_PROXY` | `DST_ADMIN_STEAM_HTTP_PROXY` | Empty; required for `custom` |

Environment variables take precedence, and overridden fields are read-only in the UI.
UI saves apply immediately. Restart the management service after manually editing its
configuration file or process environment. These settings belong to the Controller;
Agents do not need duplicate configuration.

## Metadata and file downloads

These settings cover public Workshop metadata. SteamCMD still installs and updates game
and mod files on the actual runtime node and does **not** use this dedicated proxy.
Successful searches do not guarantee successful downloads. Check the selected node's
network, disk, Steam login and job result; see [Game server management](game-installation-management.en.md).

Requests run on demand with shared concurrency limits and no idle probes. Thumbnail
disk caching is bounded and falls back to forwarding when unwritable. Metadata caches
do not modify game saves. Settings, tests, enrichment and thumbnails are covered by the
shared [OpenAPI](openapi-v2.yaml) used by the UI and AI.
