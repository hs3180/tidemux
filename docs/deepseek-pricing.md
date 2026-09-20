# DeepSeek pricing (USD)

TideMux's DeepSeek pricing examples use the [English official pricing page](https://api-docs.deepseek.com/quick_start/pricing/), checked on **2026-09-12**.
These rates apply to **deepseek-flash** (DeepSeek-V4.1-Flash) and are denominated
in USD per one million tokens. They are not currency conversions from another price list.

| Usage | Off-peak | Peak |
| --- | ---: | ---: |
| Input, cache miss | $0.15 | $0.30 |
| Input, cache hit | $0.003 | $0.006 |
| Output | $0.60 | $1.20 |

Peak hours are Monday through Friday, **01:00–04:00 and 06:00–10:00 UTC**.
All other hours are off-peak. Check the linked source for subsequent changes.

## Automatic estimate

When `base_url` uses the official `api.deepseek.com` hostname and the model is
`deepseek-flash` (including the two accepted legacy aliases), TideMux applies
the schedule automatically at request start. Explicit `prices` entries still
override the built-in schedule. Third-party compatible endpoints must provide
their own explicit prices.

For a manual override, choose the [off-peak fragment](../examples/deepseek-pricing-off-peak.json) or
[peak fragment](../examples/deepseek-pricing-peak.json). These files contain
only a `prices` object; they are not complete gateway configurations.

1. Stop the gateway and back up your local configuration at
   `~/Library/Application Support/TideMux/config.json`.
2. Merge the fragment's `prices.deepseek-flash` entry into that configuration,
   preserving other model prices, settings and Keychain references.
3. Run `tidemux doctor`, then restart `tidemux serve`.
4. Inspect this month's records in the same local ledger with
   `tidemux billing --details`.
   Add `--json` for structured output or select another period with `--from` and
   `--to`; see [billing periods](accounting.md#billing-statistics-and-download).

The entry records USD, the official source URL and the verification date/period.
The same entry supports both API protocols for this model. Cache-hit pricing maps
to `cache_read_per_million`; no separate cache-write price is invented.

The built-in schedule uses the request start time and stores the selected rate
in each successful audit's price snapshot. Successful historical records retain
their original price snapshots. DeepSeek may change prices; update TideMux if
the official schedule changes.

`configure --preset deepseek` selects the endpoint/model and credentials, and the
official endpoint automatically receives the built-in schedule. Other models
and third-party resellers require their own verified rates. See [accounting](accounting.md)
for unknown costs, cache handling and the distinction between estimates and
provider invoices.
