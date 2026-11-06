# kwtSMS for Prometheus

Send Prometheus alerts by SMS through [kwtSMS](https://www.kwtsms.com), the SMS gateway for Kuwait and the Gulf.

Prometheus does not send notifications itself: its alerts go to **Alertmanager**, which groups them and passes them on
to receivers (e-mail, chat, webhooks). `kwtsms-prometheus` is a small service that Alertmanager calls as a webhook
receiver; it turns each alert group into one SMS. You add one receiver to your Alertmanager configuration, nothing in
Prometheus itself changes.

```
Prometheus  --alerts-->  Alertmanager  --webhook-->  kwtsms-prometheus  --SMS-->  kwtSMS  -->  phones
```

## Features

- One SMS per alert group: `[FIRING:3] HighCPU critical` and the summary, or `[RESOLVED] ...`.
- Recipients per Alertmanager receiver, so your existing routes decide who gets which alert.
- Retries do not send twice: a repeated notification within 5 minutes is skipped.
- Cost guards: messages cut to 2 SMS pages by default, at most 10 numbers per alert, at most 30 SMS per minute.
- English and Arabic. Arabic is sent as Unicode (70 characters per SMS page).
- Permanent kwtSMS errors (wrong login, zero balance) are not retried; a full queue or a network error is.
- Requires a bearer token, so nobody else on the network can spend your credits.

## Requirements

- Alertmanager (webhook payload version 4; tested with the current prom/alertmanager image, 2026-11-06).
- Outbound HTTPS to `www.kwtsms.com`.
- A kwtSMS account with API access. [Sign up free](https://www.kwtsms.com/signup/).

## Run it

Docker:

```sh
docker run -d --name kwtsms-prometheus -p 9097:9097 \
  -e KWTSMS_USERNAME=your_api_username \
  -e KWTSMS_PASSWORD_FILE=/run/secrets/kwtsms_password \
  -e AUTH_TOKEN_FILE=/run/secrets/kwtsms_token \
  -v /etc/kwtsms:/run/secrets:ro \
  ghcr.io/kwtsms/kwtsms-prometheus:1
```

Or build the binary: `go build -o kwtsms-prometheus .` and run it with the same environment variables.

## Configure Alertmanager

```yaml
route:
  receiver: oncall-sms
  group_by: [alertname]
  group_interval: 5m
  repeat_interval: 4h

receivers:
  - name: oncall-sms
    webhook_configs:
      - url: http://kwtsms-prometheus:9097/alert?to=965XXXXXXXX,965XXXXXXXX
        max_alerts: 5
        http_config:
          authorization:
            credentials_file: /etc/alertmanager/kwtsms-token
```

The file `kwtsms-token` holds the same value as `AUTH_TOKEN`.

## Settings

| Variable | Default | What it does |
| --- | --- | --- |
| `KWTSMS_USERNAME` | required | kwtSMS API username (not your mobile number) |
| `KWTSMS_PASSWORD` / `KWTSMS_PASSWORD_FILE` | required | kwtSMS API password; prefer the file |
| `AUTH_TOKEN` / `AUTH_TOKEN_FILE` | required | Bearer token Alertmanager must send, 16 characters or more |
| `KWTSMS_SENDER` | `KWT-SMS` | Sender ID shown on the phone |
| `KWTSMS_TEST_MODE` | `0` | `1` = queued on kwtSMS, not delivered, no credits used |
| `MAX_PAGES` | `2` | Longest SMS, in pages |
| `MAX_NUMBERS` | `10` | Most numbers in one `to=` |
| `MAX_SMS_PER_MINUTE` | `30` | Hard cap; extra notifications get HTTP 429 |
| `LISTEN_ADDR` | `:9097` | Listen address |

`GET /healthz` answers `ok` for liveness checks.

Docker Compose `env_file`: a `$` in a value is read as a variable. If your API password contains `$`, write it as `$$`,
or use `KWTSMS_PASSWORD_FILE`, which is read as it is.

## Keep alert storms under control

Every SMS uses credits. `group_by`, `group_interval` and `repeat_interval` decide how many messages you get: group
related alerts, and repeat no more than every few hours. Route only critical alerts to the SMS receiver. Watch your
kwtSMS balance: at zero, sending stops with `ERR010` in the log.

## Troubleshooting

The service logs one line per notification. Common kwtSMS errors: `ERR003` wrong API login, `ERR010` zero balance,
`ERR024` this server's IP is not in your kwtSMS API IP whitelist, `ERR026` the country is not active on your account.
HTTP 401 in the Alertmanager log means the token does not match.

## Support

Help: https://www.kwtsms.com/support.html | FAQ: https://www.kwtsms.com/faq/ | support@kwtsms.com

## Security

See [SECURITY.md](SECURITY.md).

## License

MIT, (c) kwtSMS
