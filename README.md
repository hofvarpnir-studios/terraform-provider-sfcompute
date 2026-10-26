# Terraform / OpenTofu provider for SF Compute

A provider for [SF Compute](https://sfcompute.com) whose resources and data
sources are **generated from SF Compute's OpenAPI document**. When the API
changes, the spec snapshot is refreshed and the provider regenerated (nightly,
by CI); nobody hand-edits per-resource code.

> **Status: early, unofficial, untested against the live API.** Everything
> here is exercised offline against a mock server that is itself validated
> against the published spec, with real `terraform` and `tofu` binaries, but
> no request has been sent to `api.sfcompute.com` yet. Many of the routes
> used are in SF Compute's public preview (`/preview/v2`) and may change.

## Example

```hcl
terraform {
  required_providers {
    sfcompute = {
      source = "hofvarpnir-studios/sfcompute"
    }
  }
}

provider "sfcompute" {
  # token defaults to $SFC_TOKEN
  workspace = "sfc:workspace:acme:default"
}

resource "sfcompute_pool" "train" {
  name = "train"
  tags = { team = "ml" }
}

resource "sfcompute_firewall" "ssh" {
  name  = "ssh"
  rules = [{ direction = "ingress", protocol = "tcp", port = "22", source = "203.0.113.0/24" }]
}

resource "sfcompute_instance_template" "worker" {
  image                = "sfc:image:sfcompute:public:ubuntu-24.04-cuda-13.2"
  cloud_init_user_data = base64encode(file("cloud-init.yaml"))
}

data "sfcompute_skus" "all" {}
data "sfcompute_limits" "current" {}
```

More examples are in [`examples/`](examples) and the generated reference in
[`docs/`](docs).

## Provider configuration

| Argument         | Environment     | Default                     |                                                                                     |
|------------------|-----------------|-----------------------------|-------------------------------------------------------------------------------------|
| `token`          | `SFC_TOKEN`     | (required)                  | API token (`sk_live_...`), sent as `Authorization: Bearer <token>`. Sensitive.        |
| `base_url`       | `SFC_BASE_URL`  | `https://api.sfcompute.com` |                                                                                     |
| `workspace`      | `SFC_WORKSPACE` | none                        | Default workspace (ID or `sfc:workspace:<account>:<workspace>`) for resources and data sources that take one. |
| `allow_spending` | (none)          | `false`                     | See below.                                                                          |

Create a token with `sf tokens create --name terraform`, then grant it a role,
e.g. `sf grants create --token terraform --role admin --workspace my-workspace`
(tokens start with no permissions).

## The spending guard

Some resources cost money the moment they exist:

| Resource             | Why it is guarded                                                                 |
|----------------------|-----------------------------------------------------------------------------------|
| `sfcompute_order`    | places a buy or sell order on the market; credits are spent as soon as it fills   |
| `sfcompute_instance` | runs on, and draws down, its pool's purchased allocation; can displace lower-priority instances |

Unless the provider block sets `allow_spending = true`, **planning** the
creation (or replacement) of a guarded resource fails with an error naming the
resource and the reason, and `Create` refuses again at apply time as a second
line of defence. No request is sent to the API. There is deliberately no
environment variable for this setting, so turning spending on is always visible
in configuration and code review. Destroying guarded resources (cancelling an
order, terminating and deleting an instance) is always allowed.

The guard is driven by `generator_config.yml`: a resource is free only if it is
listed there with `spending: false`. Any resource the generator derives from a
new API endpoint is **guarded by default** until someone classifies it.

Things to know about orders: they cannot be updated, so any change to an
`sfcompute_order` plans a replacement (cancel + new order), which is itself
guarded; destroying one cancels only the unfilled remainder, and an order that
already filled is simply forgotten (with a warning).

## What is generated

7 resources and 29 data sources; [`generator/coverage.txt`](generator/coverage.txt)
lists every operation in the spec and what the provider does with it.

| Resource                         | API                                                  | Update        |
|----------------------------------|------------------------------------------------------|---------------|
| `sfcompute_workspace`            | `/v2/workspaces`                                      | PATCH         |
| `sfcompute_pool`                 | `/v2/pools`                                           | PATCH         |
| `sfcompute_firewall`             | `/preview/v2/firewalls`                               | PUT (rules)   |
| `sfcompute_instance_template`    | `/preview/v2/instance_templates`                      | replace only  |
| `sfcompute_infiniband_partition` | `/preview/v2/infiniband_partitions`                   | replace only  |
| `sfcompute_instance` (guarded)   | `/v2/instances` (terminate, then delete)              | PATCH         |
| `sfcompute_order` (guarded)      | `/v2/orders` (destroy = `POST /v2/orders/{id}/cancel`) | replace only |

Data sources: `account`, `limits`, `platform_constants`, `events`, `sku`,
`skus`, `sku_availability`, `image`, `images`, `pool`, `pools`,
`pool_transfer`, `pool_transfers`, `order`, `orders`, `orderbook_quote`,
`instance`, `instances`, `instance_ssh`, `instance_template`,
`instance_templates`, `firewall`, `firewalls`, `infiniband_partition`,
`infiniband_partitions`, `workspace`, `workspaces`, `user`, `users` (all
prefixed `sfcompute_`). List data sources fetch every page unless you set
`limit`, `starting_after` or `ending_before`.

Not generated, and why:

* **Images**: `POST /v2/images` only starts a multipart upload; a useful image
  resource needs to upload parts and complete the upload, which the generic
  runtime does not do. Images are available as data sources.
* **VPCs, subnets, tokens, grants, roles**: the docs describe them (subnets
  even mention `/preview/v2/subnets`), but no API reference page exists, so
  there is no schema to generate from.
* **Pool transfers, instance replace, image revoke/reinstate, order preview**:
  actions rather than resources.
* The separate preview document (`spec/preview/openapi.json`, older
  `/preview/v2` routes superseded by `/v2`) is kept for reference only.

## How generation works

SF Compute does not publish its OpenAPI document, but every API reference page
of its Mintlify docs embeds the slice of it for that endpoint (the operation
plus every component it references), e.g.
`https://docs.sfcompute.com/api-reference/orders/create-order.md`.

```
docs.sfcompute.com/llms.txt ─ scripts/fetch-spec ─▶ spec/openapi.json            (committed)
                                                    spec/preview/openapi.json
spec/openapi.json ─ sfcgen prep ─▶ build/openapi.json       OpenAPI 3.1 → constructs tfplugingen-openapi maps
generator_config.yml ─ sfcgen resolve ─▶ build/generator_config.yml + build/plan.json
                                         generator/coverage.txt                 (committed)
  ─ tfplugingen-openapi v0.3.0 ─▶ build/provider_code_spec.raw.json
  ─ sfcgen patch ─▶ build/provider_code_spec.json            workspace made optional (provider default)
  ─ tfplugingen-framework v0.4.1 ─▶ internal/generated/{resource,datasource}_*/  (committed)
  ─ sfcgen slim                                              keep only the schema functions
  ─ sfcgen registry ─▶ internal/provider/registry_gen.go      (committed)
  ─ tfplugindocs v0.25.0 ─▶ docs/                            (committed)
```

* **`scripts/fetch-spec`** reads `llms.txt`, downloads every `api-reference`
  page, extracts each embedded document, and merges them per source file (the
  docs are built from two distinct documents, `/openapi.json` and
  `/preview/openapi.json`, which define same-named components differently).
  Merging is key by key under `paths.<path>.<method>` and
  `components.<section>.<name>`; two pages that disagree about a definition
  fail the run with both versions shown. Output has sorted keys, so diffs
  between snapshots are minimal.
* **`sfcgen prep`** works around what tfplugingen-openapi v0.3 cannot map:
  multi-schema `allOf` (flattened), `oneOf`/`anyOf` other than "X or null"
  (string unions become a string, object unions one object with the union of
  properties), `const`, `$ref` with sibling `description`, `readOnly`/
  `writeOnly`, the constant `object` discriminator, and regex patterns Go
  cannot compile.
* **`sfcgen resolve`** derives entries from path conventions and merges the
  explicit ones in `generator_config.yml` over them (see below).
* **`sfcgen slim`**: tfplugingen-framework v0.4 also emits a model struct and
  custom Type/Value types for every nested attribute, named by attribute name
  only, so two nested attributes with the same name in one resource
  (`allocation_schedule_delta` in orders) produce Go that does not compile.
  The runtime never uses those types and the framework derives nested types
  itself, so slim keeps only the `*Schema` function.
* **`internal/runtime`** is the one hand-written CRUD engine that runs every
  generated resource and data source from its schema and its operations.

All generator tools are pinned in the [`Makefile`](Makefile) and run with
`go run pkg@version`, so their dependencies never mix with the provider's.

### Regenerating

```sh
make fetch-spec   # refresh spec/ from docs.sfcompute.com (network)
make generate     # codegen + docs (tfplugindocs needs terraform on PATH, or downloads it)
make test
```

`.github/workflows/regenerate.yml` does this nightly and opens (or updates) a
PR from `automation/regenerate` when anything changed, with the
`generator/coverage.txt` diff in the description, and enables auto-merge only
if `go test ./...` passed.

### Adding a resource or data source

Most need no configuration: `sfcgen resolve` derives

* a resource `<thing>` from `POST <p>/<things>` + `GET` and `DELETE`
  `<p>/<things>/{id}` (+ `PATCH`, else `PUT`, as the in-place update),
* a data source `<thing>` from `GET <p>/<things>/{id}`, and `<things>` from
  `GET <p>/<things>`,

under `/v2` and then `/preview/v2` (the stable route wins). A derived resource
is guarded as spending until classified, so making a new free resource
available is:

```yaml
resources:
  subnet:
    spending: false
```

Anything that does not follow the conventions is listed explicitly, in
tfplugingen-openapi's format plus a few sfcgen keys
(`spending`, `spending_note`, `before_delete`, `delete_accept_status`,
`stable`):

```yaml
resources:
  order:
    spending: true
    spending_note: it places a buy or sell order on the SF Compute market ...
    create: { path: /v2/orders, method: POST }
    read:   { path: "/v2/orders/{id}", method: GET }
    delete: { path: "/v2/orders/{id}/cancel", method: POST }
    delete_accept_status: [422]
data_sources:
  instance_ssh:
    read: { path: "/v2/instances/{id}/ssh", method: GET }
```

Names to leave out go under `skip:` with a reason. Then run `make generate`.

## How the generic runtime behaves

The rules below apply to every resource; they come from the spec, not from
per-resource code.

* **Requests** send the top-level body fields the operation documents, taken
  from configuration; unset optional fields are omitted. Path parameters
  (`{id}`) come from state and are escaped as one segment; IDs and resource
  paths (`sfc:pool:acme:prod:train`) both work.
* **Replacement**: attributes that are not in the update operation's body (or
  all of them, if there is no update operation) force replacement.
* **Apply results**: configured values are kept exactly; unknown values
  (computed attributes, unset optional ones, nested computed parts such as a
  firewall rule's default `source`) are filled from the response.
* **Refresh** takes computed attributes from the API. Configurable
  attributes are taken from the API too (so drift in, say, a pool's name or
  tags shows up), except attributes that cannot be updated in place or that
  the API echoes in a different shape than it accepts (a workspace resource
  path in, `{id, name}` or a bare name out; `cloud_init_user_data` not echoed
  at all), which keep the applied value. `null` and empty collections are not
  drift. A 404 removes the resource from state.
* **Plan**: unset optional+computed attributes keep their prior value instead
  of becoming "known after apply"; so do `id` and `created_at`. A consequence
  (standard for optional+computed attributes): removing an argument such as
  `tags` from configuration leaves the remote value as it is; set it
  explicitly (`tags = {}`) to clear it.
* **Delete** runs any `before_delete` actions (terminate an instance), treats
  404 as done, retries 409 Conflict for up to 5 minutes, and treats
  configured statuses (422 for an order that cannot be cancelled) as done with
  a warning.
* **Retries**: 429/502/503/504 and network errors are retried with backoff
  (honouring `Retry-After`) for every method except POSTs without an
  `Idempotency-Key` parameter in the spec; for POSTs that have one (orders), a
  key is generated per create and reused across retries.
* **Import** by ID or resource path: `terraform import sfcompute_pool.train
  pool_...`. Reference attributes the API returns as summary objects
  (`workspace`, `pool`, `image`, `sku`) are imported as IDs; if your
  configuration uses resource paths for them, the next plan wants to replace
  the resource. Set them to the imported IDs (or use
  `lifecycle { ignore_changes }`) after importing.

## Development

Go is not assumed to be installed; with nix:

```sh
nix shell nixpkgs#go nixpkgs#gnumake nixpkgs#opentofu -c make test
```

Tests run entirely offline:

* `internal/specmerge`: extraction and merging (conflicts, ordering, the
  fetcher against a fake docs site).
* `internal/codegen`: spec preparation, derivation, config validation,
  code-spec patching, slim, registry rendering, and that the committed spec
  and config still resolve.
* `internal/mockapi`: an in-memory SF Compute API that validates every
  request it receives and every response it sends against
  `spec/openapi.json`; any violation fails the test that caused it.
* `internal/runtime`: the CRUD engine driven like Terraform drives it, with
  Terraform's consistency rules checked after every apply.
* `internal/provider`: the full provider schema, plus terraform-plugin-testing
  runs with a real CLI against the mock (create, no-op re-plan, update,
  import verification, replacement, data sources, the spending guard). These
  use `TF_ACC_TERRAFORM_PATH`, else `terraform` or `tofu` on `PATH`, and are
  skipped if neither exists.

To try a local build: `go build -o ~/.terraform.d/dev/terraform-provider-sfcompute .`
and add a `dev_overrides` block for `hofvarpnir-studios/sfcompute` pointing
at that directory to your CLI configuration.

## Releasing

Push a `v*` tag. `.github/workflows/release.yml` runs the tests, imports the
signing key from the `GPG_PRIVATE_KEY` and `PASSPHRASE` secrets, and runs
goreleaser, which publishes per-platform zips, a `SHA256SUMS` file with its
detached signature, and `terraform-registry-manifest.json` (protocol 6.0) as
the Terraform and OpenTofu registries expect.

Releases are signed with the RSA-4096 key in [`signing-key.asc`](signing-key.asc),
fingerprint `5BDC E571 32CB E074 E6AF  6CF9 68E0 5251 9674 2213`. The private
key exists only in the repository's Actions secrets; if it's ever lost, generate a
new one, update the secrets and this file, and add it to both registries.

The nightly workflow relies on repository settings that are in place: "Allow GitHub
Actions to create and approve pull requests", "Allow auto-merge", and a `main`
ruleset requiring the `test` workflow's checks.

## License

[MPL-2.0](LICENSE).
