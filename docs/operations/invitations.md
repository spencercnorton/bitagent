# Member invitations

Invitations are optional application-membership grants for verified identities
from your authentication service. They do not create an account system, enroll
a VPN device, issue a peer lease, verify media or provision a seed. Configure and test those separate
boundaries before admitting traffic to a private library.

The feature defaults to disabled. These settings are startup-only:

| Setting | Default | Behavior |
|---|---|---|
| `PRIVATE_INVITATIONS_ENABLED` | `false` | Requires authenticated private indexing or separately enabled site registration, and a verified identity proxy. |
| `SITE_REGISTRATION_ENABLED` | `false` | Invite-only profile enrollment before private indexing. Requires invitations and the protected enrollment bridge; does not open private routes. |
| `SITE_REGISTRATION_ISSUER_ADMISSION_ENABLED` | `false` | Allows a verified human OWNER to deliberately admit their own invitation issuer on the canonical operator origin. Requires site registration with private indexing and checkout off. |
| `INVITATION_ANNUAL_ALLOWANCE` | `3` | Free invitations generated per member per UTC calendar year. |
| `INVITATION_OWNER_IDS` | empty | Comma-separated exact existing SSO subject IDs with unlimited annual generation; a role name or machine credential grants no exemption. |
| `INVITATION_PRICE_USD_CENTS` | `5000` | Display/ledger price in USD; the Checkout adapter requires exactly `5000`. |

The configured HTTPS `PRIVATE_INDEXER_URL` supplies the share origin. Never use
request headers to construct it. Preserve existing subject IDs across upgrades
and SSO providers; changing an identity ID would split its existing ledgers.

Approved members open `/invitations`. Generating a link permanently consumes
one annual allowance or purchased credit. Revocation, seven-day expiry, failed
delivery and an unavailable mint response do not refund that allowance. Do not
automatically retry an uncertain mint response: another attempt could generate
and charge another invitation. The calendar allowance resets on January 1 UTC;
paid credits are a separate ledger and do not reset annually. Unlimited-owner
generation is still recorded, so removing an exemption retains its history.

The server generates 256 bits of opaque token entropy, stores only its SHA-256
hash, and returns the raw token only in the successful mint response. Account
lists contain the most recent 100 links' status and timestamps, never tokens,
hashes, issuer details or recipient IDs. Save the share link when it is shown;
the server cannot retrieve it later.

Share links use `/invite#TOKEN`, with the token in the fragment rather than a
query or path. The landing page clears the fragment promptly and keeps the
token in memory, without browser storage. These pages and APIs use
`Referrer-Policy: no-referrer` and `Cache-Control: no-store`. Keep proxy request
body/debug logging disabled too; a redeem or preview JSON body contains the
token. The unauthenticated preview is rate-limited and returns the same
unavailable shape for malformed, unknown, expired, revoked and consumed tokens.
A valid preview discloses expiry only.

With the enrollment bridge disabled, direct redemption requires a verified human
proxy identity. This existing path does not collect or enforce a profile email.
The transaction rechecks that the issuer remains active and that the token is unexpired and unused,
then records consumption and the exact recipient's membership together. A
suspended identity cannot reactivate itself with an invitation; its membership
remains operator-managed. An existing active member cannot consume an unused
link. The same recipient can acknowledge an already redeemed link while still
active, allowing recovery from a lost successful response. Another principal
cannot reuse it. Suspending an issuer revokes all their pending links in the
same transaction; later reapproval does not revive those links.

Your upstream SSO must let an invitee authenticate without prematurely
granting library or network access. An installation that admits only existing
SSO identities needs a separately reviewed onboarding path. The generic
redemption endpoint does not accept a caller-supplied identity or a machine
key as a substitute for the trusted proxy's human identity.

## Optional profile enrollment bridge

Recipients can open `/invite` and enter an invite code, or follow their fragment
link. The manual form accepts only the exact generated code, clears it before
preview, and sends it in a request body. Invalid or unavailable codes do not
create an account or reveal who issued the invitation. Codes never enter a query
string, browser storage or status message.

To offer site registration while `PRIVATE_INDEXER_ENABLED=false`, explicitly set
`SITE_REGISTRATION_ENABLED=true`, `PRIVATE_INVITATIONS_ENABLED=true` and
`PRIVATE_INVITATION_BRIDGE_ENABLED=true`. Keep
`PRIVATE_INVITATION_CHECKOUT_ENABLED=false`. Authentication and a verified
identity proxy remain required. `PRIVATE_INDEXER_URL` must be the exact canonical
HTTPS public origin, without a port, trailing slash, query or fragment, and its
host must be in `PUBLIC_LIBRARY_HOSTS`. Only that exact public Host serves the
registration and invitation endpoints; aliases and operator hosts do not.

This mode uses the existing membership and invitation ledgers. It neither
creates an initial issuer nor changes account IDs, public keys, usage or
preferences. An invitation still requires an active issuer; owner allowance
exemptions do not create membership. Suspension and the signed enrollment
service's identity-history/profile checks remain mandatory. All private
catalogue, tracker, transfer and private-key routes remain unavailable until
their separate feature is enabled.

### Admit the first invitation issuer

An empty membership ledger cannot mint an invitation. To admit an existing
owner without enabling private indexing, explicitly enable
`SITE_REGISTRATION_ISSUER_ADMISSION_ENABLED` and set `OPERATOR_UI_URL` to its
exact HTTPS operator origin, without a port, path, trailing slash, query or
fragment. Preserve the existing proxy proof, transport allowlist and upstream
grant policy. The feature requires a verified human OWNER; a dashboard key,
owner allowance setting or role on the public host cannot admit a member.

Sign in on that operator origin and open **Invitation issuer**. Review the
actual signed-in account, then choose **Enable invitations for my account**.
Only that explicit action inserts an absent membership row, with the verified
account as its actor. A status read, page load or owner configuration never
admits anyone. An active membership is unchanged; an inactive membership is
refused and needs separate operator review. The request cannot choose another
account or reactivate a suspension.

Then open **My invitations** on the public origin to generate an invite code.
Keep the same existing SSO subject namespace on both origins. This action
creates no authentication account, upstream service grant, personal API key or
private permission. All private administration, catalogue and tracker routes
remain unavailable. Human owner sign-in and invitation acceptance are deployment
checks, not consequences of enabling the setting.

The reverse proxy must permit anonymous access only to the canonical `/invite`
page, its fixed local static assets and the JSON POST preview endpoint. Keep
token/body logging disabled. The operator bootstrap endpoint still requires
its existing transport/proxy proof and purpose HMAC; do not expose it as a public
registration endpoint. An application setting does not configure proxy ingress.

For registration previews the proxy must overwrite `X-BitAgent-Peer-IP` with
the original connection address. Only a transport-allowlisted proxy carrying
the existing shared proof can select a rate bucket this way. Require one
canonical literal IPv4 or IPv6 address; invalid or untrusted headers use the
transport-peer bucket. This rate-limit input grants no identity or membership.

An authentication service can use a separate bootstrap protocol when an invitee
has not yet acquired an approved identity. All bridge settings are startup-only
and the feature defaults to disabled:

| Setting | Behavior |
|---|---|
| `PRIVATE_INVITATION_BRIDGE_ENABLED` | Requires authenticated private invitations and the verified proxy. |
| `INVITATION_BRIDGE_URL` | Exact canonical HTTPS operator-origin `/api/invitations/bootstrap`, without port, query, fragment or credentials. |
| `INVITATION_BRIDGE_SECRET_FILE` | Distinct 32–4096-byte regular POSIX file, no symlinks/hard links or group/other permissions; exact file bytes are the HMAC key. |
| `INVITATION_BRIDGE_SUBJECT_FORMAT` | Startup-only `legacy-negative` (default) or `principal`. Both require an exact positive integer principal; the SSO service must use the same format. |
| `INVITATION_SIGN_IN_URL` | Fixed canonical HTTPS SSO `/invitations/start` form target; only the invitation landing page permits that origin in `form-action`. |

When the bridge is enabled, `/invite` immediately prompts recipients to create
a profile and sends every valid invitation through the token-only form,
including recipients with an existing browser session. A successful anonymous
preview triggers exactly one automatic top-level handoff. If browser navigation
is blocked, an explicit Create your profile button remains available; there is
no automatic retry. The token remains only in memory until handoff or page exit.
Direct human
`POST /api/invitations/redeem` returns `403` in this mode. An invalid or unavailable
enrollment configuration cannot fall back to direct redemption.

The authentication service must collect a required email before granting
membership. It may offer site-local username/password accounts with the exact
provider identifier `bitagent-local` alongside optional SSO. It owns credential
validation, password hashing, profile completion and project-only login policy;
BitAgent neither receives passwords nor treats an email as identity authority.
The signed bridge accepts that provider only through the same protected machine
boundary and exact principal/subject binding as the supported SSO providers.

The upstream machine location must restrict its callers to the reviewed SSO
service, preserve the verified proxy proof, and clear cookies, browser
identity, authorization and fetch-origin headers. The public library host
hides the bridge. A dashboard key or human operator identity cannot call it.

Requests and responses use separate purpose-signed HMAC-SHA256 messages.
`X-Invitation-Client` is exactly `bitagent-auth`; time is canonical integer UTC,
nonce is 192-bit URL-safe text and signature is lowercase SHA-256 hex. The
canonical message in `invitation_bridge.canonical` binds origin, method, path,
operation, time, nonce and exact body digest. A response additionally binds the
current request body digest and HTTP status. Validate the exact response bytes,
current nonce, original expiry and bounded ten-second call before using it.
Never save a success response as future membership authority.

`start` creates a bounded ten-minute enrollment without consuming the link or
granting membership. The authentication service must bind its host-only secure
cookie, existing OAuth state or site-local registration and verified account
subject, then reserve a stable unapproved principal. `redeem` derives the configured account ID from that
reserved positive principal and commits membership and consumption together. `status`
reconciles an already bound enrollment; it cannot bind an arbitrary principal.
Recovery requires the same provider/principal/subject binding and a still
active issuer and member, valid invitation and unexpired enrollment. All
nonces and active enrollments have explicit capacity and expiry bounds.

The default `legacy-negative` format derives a negative decimal account ID.
The opt-in `principal` format derives `principal:<positive integer>`. Neither
accepts provider IDs, caller-supplied account IDs or a fallback format. Each
enrollment stores its startup format; schema upgrades mark existing enrollments
as legacy, and changing the format refuses pending or consumed enrollments in
the previous format. A new redemption also refuses any membership row in the
other format, including a suspension or revocation record. Already consumed
same-subject recovery still requires that exact account to remain active.

Account data belongs to exact subject strings. Enabling `principal` requires
matching trusted SSO headers and a separately reviewed migration for existing
memberships, keys, usage, preferences and invitation/payment ledgers. This
setting does not migrate, merge, reactivate or grant access to legacy accounts.
Verify existing account continuity before enabling it on a populated site.

Only fresh membership authority permits the SSO service's separately reviewed
project-capability activation. It must preserve denial history and refuse
reactivating revoked grants. The two databases do not form a distributed
transaction; partial enrollment must remain recoverable and fail closed.
Membership is authorized at the transaction, and a later suspension can
invalidate it before the response arrives. Every protected request and peer
lease must still enforce current membership. This bridge does not approve
provider identities itself, create SSO cookies or enroll network devices.

## Payment integration

Hosted Checkout defaults to unavailable. Enable it only after qualifying the
configured merchant, fixed one-time USD 50 Price, restricted key capabilities
and genuine signed sandbox events. No billing key is discovered or enabled
automatically. These additional settings are startup-only:

| Setting | Default / requirement |
|---|---|
| `PRIVATE_INVITATION_CHECKOUT_ENABLED` | `false`; requires authenticated invitations. |
| `INVITATION_STRIPE_API_KEY_FILE` | Protected regular file containing a mode-matching `rk_test_`/`rk_live_` restricted key, or `sk_test_`/`sk_live_` key. |
| `INVITATION_STRIPE_WEBHOOK_SECRET_FILE` | Separate protected regular file containing the endpoint's `whsec_` signing key. |
| `INVITATION_STRIPE_ACCOUNT_ID` | Exact merchant `acct_` ID, verified by the account API. |
| `INVITATION_STRIPE_PRICE_ID` | Exact active one-time `price_` ID for one USD 50 credit. |
| `INVITATION_STRIPE_LIVE_MODE` | `false`; mode must match keys, Price, events and retrieved objects. |

Keys are not exposed to browsers. Grant only required Account, Price, Checkout
Session/line-item, PaymentIntent, Charge and Dispute reads plus Session creation;
missing capabilities leave payments pending instead of granting credit.
Account and Price proofs expire after five minutes or a failed observation.
Checkout uses fixed quantity one, card payment, no recurring charge, promotion,
shipping, automatic tax or adaptive pricing. Unlimited owners cannot initiate
Checkout; already paid receipts still reconcile if owner configuration changes.

`POST /api/account/invitations/checkout` requires human SSO and the same account
binding/JSON CSRF boundary as invitation generation. An explicit request uses a
canonical UUID `requestId`. The server persists the order before contacting
Checkout. The immutable provider parameters and idempotency key survive lost
responses, cancellation and process restart. Only one unresolved order is
allowed per account. Retry the same request explicitly; never automatically
replace its key or start another charge. An unknown outcome older than 23 hours
requires operator reconciliation, because provider idempotency keys can expire.
Only a provider-confirmed expired/unpaid Session releases an open order.

The server returns a validated `checkout.stripe.com/c/pay/...` HTTPS URL only
for a known open Session. It keeps Stripe's authentic fragment. The browser
return and `GET /api/account/invitations/orders` only report state; neither
grants credit. Orders expose up to 20 own records and the original request UUID
for recovery, without provider object IDs, keys or stored Checkout URLs.
Human invitation/Checkout JSON bodies have an absolute ten-second streamed
deadline and 8 KiB limit. Bound TCP header ingress separately at the HTTP server
or trusted proxy; an ASGI application cannot time headers before it receives
them.

The exact `POST /api/invitations/stripe/webhook` is the only unauthenticated
payment route. Configure the proxy's exception for that exact path and method;
keep other human and operator routes protected. Require API version
`2025-08-27.basil`, matching the adapter. The route bounds the raw body to 256 KiB
and validates timestamped HMAC-SHA256 before decoding strict JSON. It persists a
receipt before acknowledging delivery. A bounded worker verifies the merchant,
Session, exact line item, PaymentIntent and successful Charge through fixed
TLS API routes before granting one credit to the order's original SSO subject.
Duplicate or reordered events cannot produce another credit. No browser-supplied
identity, price, amount or success claim is accepted as payment authority.
Verified unrelated merchant events are durably ignored. Malformed or unknown
invitation-purpose orders are quarantined for operator reconciliation without
credit; an owned held order continues to block another charge. Transient
provider failures move behind never-attempted receipts. Each cycle selects at
most 100 records and uses four workers, preventing a fixed oldest-event window
from indefinitely excluding valid payments.

Any verified positive refund or dispute permanently invalidates that credit and
revokes its pending paid invitation. A redeemed membership remains managed by
the membership system. Late paid events cannot restore invalidated credits;
winning a dispute does not automatically restore one. Paid invites permanently
consume a specific credit, including on expiry or ordinary revocation. Older
unmapped paid invitations remain spent during schema upgrades.

`invitations.fulfill_paid_credit` remains an internal trusted ledger helper,
not an HTTP payment-verification boundary. It grants neither membership nor
peer access. Mocked HTTP/SQLite tests do not prove real Stripe delivery, key
permissions or live billing. Qualify genuine sandbox delivery, duplicates,
reordering, refund/dispute recovery and proxy reachability before enabling.

The adapter pins the [official Stripe API version](https://github.com/stripe/stripe-python/blob/v12.5.1/stripe/_api_version.py)
and uses its [Checkout parameters](https://github.com/stripe/stripe-python/blob/v12.5.1/stripe/checkout/_session_service.py).
Review [webhook signatures](https://docs.stripe.com/webhooks) and
[idempotency retention](https://docs.stripe.com/api/idempotent_requests) when
changing this boundary.
