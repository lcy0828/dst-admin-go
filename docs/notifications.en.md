# Email and Webhook notifications

Open **System settings → Notifications**. Saved changes take effect immediately.

The Controller sends notifications for local and connected Agent management tasks. Standalone Agents do not need SMTP settings. SMTP and Webhook endpoints must be accessible from the Controller host or container.

## Email

1. Expand **Mail server (SMTP)**, enter the server, port, sender address and credentials if required, then save. Common ports are `465` for TLS and `587` for STARTTLS. Unauthenticated LAN relays can leave the username blank.
2. Test the SMTP connection. This checks connection, TLS and authentication without sending email.
3. **Add channel → Email**, enter a name and recipients, then choose events. Up to 20 recipients per channel, one per line or comma-separated.
4. Save the channel and **Send test**. Inspect the actual result in delivery history.

`Accepted` means the mail server accepted the message; check the inbox and spam folder. Channel sends use saved SMTP settings, including when the SMTP form contains unsaved edits.

## Webhooks

1. **Add channel → Webhook**, enter a name, URL and events.
2. Generic endpoints can use the default JSON template. For WeCom, DingTalk or Feishu bots, select the provider under **Request and message template** and use its bot URL.
3. Custom endpoints support `POST` / `PUT`, JSON / plain text, custom headers and message templates.
4. Save and **Send test**. Bot presets also validate the provider's response code.

Example headers:

```json
{ "Authorization": "Bearer YOUR_TOKEN" }
```

Saved endpoint URLs and headers are never returned in plaintext. Leave them blank when editing to preserve them; enter `{}` to clear headers. Dynamic bot signing is not supported yet. Use an endpoint accepting a fixed token or a forwarding service.

## Events and filters

| Event | Trigger |
| --- | --- |
| Room start and stop results | Room management jobs finish, including partial failures |
| Backup and restore results | Manual or scheduled backup, restore and pruning jobs finish |
| Game installation and update results | Local or Agent installation, adoption and update jobs finish |
| Mod operation results | Mod management jobs finish; successful routine update checks remain silent |
| Agent disconnects and reconnects | Observed connection state changes; first observation and Controller restarts do not send a fleet-wide status report |
| Sign-in attempts blocked | Repeated failed sign-ins reach the configured threshold |
| New management service release | Existing update checks find a new release; deduplicated per channel/version while history is retained, without adding a release polling loop |
| Custom messages | Manual sends from this page or the custom notification API |

Channels have independent subscriptions and enable switches. **Failures and warnings only** skips successes, reconnects, new releases and custom messages. Explicit test sends bypass this filter. Empty-room updates that do no work do not send success notifications.

Task notifications include operation kind, status, room/world identity, task ID and error code. Raw command output and arbitrary logs are excluded. Independent game crash detection and player join/leave events are not included.

## Custom message templates

Use double-brace variables such as `{{title}}`. In JSON templates, put variables inside string values. Message quotes and newlines are escaped automatically.

```json
{
  "text": "{{title}}\n{{message}}",
  "event_id": "{{id}}",
  "room": "{{roomId}}"
}
```

| Variable | Value |
| --- | --- |
| `id` | Unique event identity for receiver deduplication |
| `event` | Event kind, such as `backup.result` |
| `title` / `message` | Title / body |
| `status` | Task or event status |
| `roomId` / `targetId` / `jobId` | Room / node / task identity when available |
| `timestamp` | UTC event time |

**Send custom message** targets enabled channels subscribed to custom messages. `POST /api/v2/system/notifications/actions/send` uses the existing session, CSRF and idempotency requirements. See [OpenAPI](openapi-v2.yaml).

## Delivery history and persistence

One background worker sends messages without blocking game jobs. It sleeps when the queue is empty. The page refreshes results only while its displayed list contains pending sends.

Webhook connection errors, HTTP `429` and `5xx` are retried up to 3 total attempts. Retries reuse `X-DST-Event-ID` for receiver deduplication. Email is not automatically retried. Sends interrupted by Controller restart become failed; use **Send again** if needed. The recipient may already have received an interrupted send, so duplicates are possible.

Limits: 32 channels, 1000 pending deliveries and 2000 recent completed deliveries. Deleting a channel retains history and cancels sends that have not started; an in-flight request may complete.

Include the database and `notifications/webhook.key` in the configuration directory when backing up management data. If the key is lost, restore the matching key or reenter each endpoint URL and all headers (`{}` for none). SMTP credentials retain their existing system configuration storage.

## Software release alerts

Subscribe a channel to management service or Agent release alerts. These subscriptions bypass the failures-only filter.

Background release checks default to every six hours and run only with an enabled release subscription. Disable them or choose an interval of 1–168 hours in notification settings. The browser may remain closed. Checks reuse the Controller's saved update source/proxy and registered Agent version information; they never start or query game worlds.

Each channel queues a component, node and version once, independently of service restarts or delivery-history cleanup. Inspect and retry failed deliveries in notification history. An interrupted send may already have reached the recipient, so manual retries can duplicate it.
