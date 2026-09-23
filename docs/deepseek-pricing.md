# DeepSeek pricing (USD)

TideMux's built-in DeepSeek rates use the [English official pricing page](https://api-docs.deepseek.com/quick_start/pricing/), checked on **2026-09-12**.
They intentionally use the peak rates for **deepseek-flash** (DeepSeek-V4.1-Flash),
denominated in USD per one million tokens. TideMux does not switch rates by clock
or replace the provider's statement.

| Usage | Peak rate |
| --- | ---: |
| Input, cache miss | $0.30 |
| Input, cache hit | $0.006 |
| Output | $1.20 |

Check the linked source for subsequent changes. Historical audit records retain
the price snapshot used when they were written.

## Configure with the API key

When `configure` receives a matching DeepSeek endpoint and model, it stores these
rates in the provider profile alongside the API key reference. The `budget`
command never edits or asks for pricing.

For a custom provider or a deliberate override, set rates on `configure`:

```sh
tidemux configure \
  --base-url https://provider.example/v1 \
  --model your-model-id \
  --pricing-input-cache-hit 1 \
  --pricing-input-cache-miss 2 \
  --pricing-output 4 \
  --pricing-source https://provider.example/pricing \
  --pricing-version 2026-09-20
```

Rates are per million tokens. All three rates are required. `--pricing-currency`
defaults to `USD`; source and version default to `manual-cli` and `manual`. The
same entry applies whichever provider protocol TideMux detects for the
configured model.

Prices are stored in the local provider profile, not in Keychain and not in the
budget section. DeepSeek may change prices; update TideMux or use explicit custom
rates if the official schedule changes.

See [accounting](accounting.md) for unknown costs, cache handling and the
distinction between estimates and provider invoices.
