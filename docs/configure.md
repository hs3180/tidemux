# Configure TideMux from Terminal

No manual Keychain Access setup is required. `configure` reads your API key
without echo and automatically creates a different local gateway credential.
Neither secret is accepted as a command-line argument or written into JSON.

## DeepSeek Flash: three commands

Run the installed CLI (or replace `tidemux` with `./tidemux` for an extracted binary):

```sh
tidemux configure --preset deepseek
tidemux doctor
tidemux serve
```

1. `configure` selects OpenAI format, `https://api.deepseek.com`, and
   `deepseek-flash`. At **API key (hidden)**, paste your DeepSeek key and press Enter.
   Nothing appears while entering the key; this is expected.
2. It generates a gateway token, stores both credentials under unique accounts
   in your login Keychain, creates the state directory, and saves a mode-0600
   configuration containing only references. It verifies read-back and directory
   access. This step makes **no API request**.
3. `doctor` confirms local readiness. `serve` listens at `127.0.0.1:8787`;
   keep this terminal open. Stop with Control-C.

Default files:

- Config: `~/Library/Application Support/TideMux/config.json`
- Ledger: `~/Library/Application Support/TideMux/ledger.db`

The CLI resolves its default path; manually supplied JSON paths still need
absolute paths (no `~` expansion). Query requests with `tidemux ledger`.

## If the login keychain is locked

Run this in **your Terminal**, not in chat:

```sh
security unlock-keychain
```

At that command's password prompt, enter your **Mac login password**, not your
API key. The system handles the password interactively; do not add a `-p` value
to the command or put the password in a script. Then rerun `tidemux configure`.
If macOS displays a Keychain access dialog, verify it is for the `security`
system utility invoked by your command before allowing access.

## DeepSeek via Anthropic format

Use a separate profile so the first configuration remains available:

```sh
tidemux configure --preset deepseek --protocol anthropic \
  --config "$HOME/Library/Application Support/TideMux/anthropic.json"
tidemux serve --config "$HOME/Library/Application Support/TideMux/anthropic.json"
```

Stop the other server first: both profiles default to port 8787. This selects
`https://api.deepseek.com/anthropic/v1`; the gateway appends `/messages`.
Paste the same DeepSeek API key when prompted. Each profile gets separate
Keychain references and a local gateway token.

## Any other compatible API

```sh
tidemux configure --protocol openai \
  --base-url https://your-provider.example/v1 \
  --model your-model-id \
  --config "$HOME/Library/Application Support/TideMux/custom.json"
```

Use `--protocol anthropic` for an Anthropic-format API. Include the endpoint's
version/path prefix. Model IDs are not limited to a preset. `--max-in-flight 2`
changes the explicit concurrency cap. Inspect other options with
`tidemux configure --help`.

## Update an existing profile

By default the command refuses to overwrite an existing file. To intentionally
replace it, stop the server and repeat the configure command with `--replace`.
New Keychain accounts are generated; old credentials and ledger data are retained
so other profiles are not broken. Remove unused old entries later in Keychain
Access if desired. A failed setup rolls back newly saved entries; the previous
configuration remains in place.

## Connect clients and test

The local credential is separate from the upstream key. Clients send the local
token as Bearer auth (Anthropic clients can use `x-api-key`). Its Keychain
service/account are in `access_token_keychain` in your config; use Keychain
Access to reveal/copy it locally if the client needs manual entry. Do not paste
credentials into chat or put them into shell history.

For a minimal authorized DeepSeek call, the source helper retrieves the local
credential itself (no token copy needed):

```sh
python3 scripts/verify_live.py \
  --config "$HOME/Library/Application Support/TideMux/config.json" \
  --disable-thinking --output /tmp/tidemux-live-openai.json
```

A verified price entry is needed to finish cost reconciliation. `configure`
intentionally does not guess current prices: until you add them, ledger cost
is `null`. The helper can report successful usage reconciliation but stop at
unknown cost. See [pricing and live verification](demo.md). Never repeat a live
request solely to fix a documentation step without considering its API cost.
