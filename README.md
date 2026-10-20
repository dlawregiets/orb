# orb

A terminal UI for browsing OpenRouter's model catalog and curating the models
that OpenCode may use through OpenRouter.

## Build and install

```sh
make            # build ./build/orb
make install    # install into $GOBIN, or $GOPATH/bin
make install BINDIR=/usr/local/bin
```

`make test`, `make lint`, `make vet`, and `make clean` cover the rest of the
usual workflow.

## Usage

```sh
orb
orb --config ./opencode.json
```

By default orb edits the global OpenCode configuration:
`$XDG_CONFIG_HOME/opencode/opencode.json` (falling back to
`~/.config/opencode/opencode.json`). `opencode.jsonc` is used if that is the
file that exists.

`OPENROUTER_API_KEY` is used for catalog and endpoint requests when present.
OpenRouter only returns latency and throughput statistics to authenticated
requests; uptime and pricing work without a key.

## Keys

| Key | Action |
| --- | --- |
| `/` | Search |
| `f` | Show free models only |
| `o` | Show allowed models only |
| `m` | Cycle modality filter |
| `y` | Knowledge cutoff filter |
| `enter` | Open model details |
| `space` | Allow or remove a model |
| `p` | Edit upstream provider routing |
| `s` | Save the OpenCode config |
| `r` | Refresh the OpenRouter catalog |
| `q` / `esc` | Back or quit |

In the routing view, `space` includes or excludes an upstream provider,
`J`/`K` reorder it, `a` toggles `allow_fallbacks`, and `i` adds a provider
slug manually. Manual entry is useful for OpenRouter aliases, which do not
expose an endpoint list.

Providers are shown with their p50 time to first token (`TTFT`), p50 output
throughput (`t/s`), and 30-minute uptime (`Up 30m`). The line below the table
adds the 5-minute and 1-day uptime plus p90 latency and throughput for the
selected provider. Providers with several endpoints are averaged, and
latency/throughput only appear when `OPENROUTER_API_KEY` is set.

## How the OpenCode config is managed

OpenCode has no model-level allowlist, so orb uses a dedicated provider:

```jsonc
{
  "providers": {
    "openrouter-curated": {
      "canonical": "openrouter",
      "env": ["OPENROUTER_API_KEY"],
      "models": {
        "~deepseek/deepseek-flash-latest": {
          "body": {
            "provider": {
              "order": ["inference-net"],
              "allow_fallbacks": false
            }
          }
        }
      }
    }
  },
  "policies": [
    { "action": "provider.use", "resource": "openrouter", "effect": "deny" },
    { "action": "provider.use", "resource": "openrouter-curated", "effect": "allow" }
  ]
}
```

Orb seeds the allowlist from any existing `providers.openrouter.models`
entries, copies their request settings, and rewrites `openrouter/...` model
references in the same file (the default model, agents, and commands) to
`openrouter-curated/...` when the model is allowed.

The raw `openrouter` provider is denied by policy but left in place so its
settings are not lost. References in external agent or command files are not
rewritten; point those at `openrouter-curated/...` if they use a curated model.

Unrelated settings and comments (in JSONC files) are preserved when orb saves.
Orb sets `env: ["OPENROUTER_API_KEY"]` on the curated provider because
`canonical` inherits the catalog but not credential lookup. If you authenticate
through a saved `/connect` account rather than the environment variable, the
curated provider may need its own connection.

## License

Apache License 2.0. See [LICENSE](LICENSE).

