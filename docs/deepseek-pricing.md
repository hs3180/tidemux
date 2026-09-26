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

## Set a provider price

The 0.2.0 CLI keeps rates with a provider and model. After adding a
provider and obtaining its reference from `provider list`, set or override its
price with:

```sh
tidemux provider pricing set REF deepseek-flash \
  --input-cache-hit 0.006 \
  --input-cache-miss 0.30 \
  --output 1.20 \
  --currency USD \
  --source https://api-docs.deepseek.com/quick_start/pricing/ \
  --version 2026-09-12
```

All three rates are required and are per million tokens. Prices are stored in
the local provider profile, separately from its budget policy. The 0.1.1 CLI
predates provider-scoped pricing.

DeepSeek may change prices; update TideMux or use explicit custom rates if the
official schedule changes. See [accounting](accounting.md) for unknown costs,
cache handling and the distinction between estimates and provider invoices.
