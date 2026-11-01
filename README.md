# imgserver

One file API for your auth, support, project, and other backends. Backends share **one signing key** and issue short-lived, scoped JWTs. Browsers upload directly to imgserver, which stores files in S3-compatible storage and returns metadata plus a signed receipt. Your backend verifies the receipt and stores the file ID.

The service includes public avatars for users and projects, private attachments, image resizing, signed downloads, SQLite metadata, and Swagger UI. All project files live in this directory.

## Run locally

Requires Go 1.27 or later.

```sh
go run . init
go run . serve
```

`init` creates a private `config.json` with a random signing key, local object storage, and the default profiles. It refuses to overwrite an existing configuration. Runtime data goes in `.data/`. The generated configuration is ignored by Git.

- Swagger UI: <http://localhost:18080/docs>
- OpenAPI: <http://localhost:18080/openapi.json>
- Readiness: <http://localhost:18080/readyz>

Swagger UI loads pinned assets from unpkg.com, so the browser needs access to that CDN. The API specification is embedded in the binary and served locally.

Build a standalone binary with `go build -o imgserver .`. Every command accepts `-config path/to/config.json`.

## Upload profiles

| Profile | Access | Input limit | Processing | Alias |
| --- | --- | --- | --- | --- |
| `avatar` (users and projects) | Public | 5 MiB | Square crops: 32, 64, 128, 256 | Lowercase MD5 of identifier |
| `ticket` | Private | 25 MiB | Original bytes, any file type | None |
| `ticket-image` | Private | 10 MiB | Fit within: 128, 512, 1024 | None |

Profiles are configured in `config.json`; add new purposes without changing application code. `max_bytes` limits uploaded bytes. An upload grant can set a smaller `max_bytes`. Visibility and available sizes come from the server profile and cannot be changed through multipart fields.

Image profiles accept JPEG, PNG, GIF and WebP. The service checks decoded dimensions before decoding, applies JPEG EXIF orientation, strips embedded metadata by re-encoding, and generates all configured sizes during upload. JPEG remains JPEG; other formats become PNG, preserving transparency. GIF animation becomes its first frame. The original image URL serves the normalized image, and the response size/checksum describe those normalized bytes. Encoded image outputs are capped at 64 MiB each. `ticket` preserves the original bytes without image processing; use `ticket-image` for private previews.

Avatars crop to the requested square size and can upscale. Other profiles preserve aspect ratio without upscaling. Only configured sizes are accepted, keeping processing and storage bounded. `max_pixels` defaults to 25 million and `max_concurrent` to four simultaneous uploads. Size your memory and temporary disk for concurrent image decoding and encoding, or lower these limits.

## Backend integration

`client.mjs` is an optional, dependency-free Node.js helper for issuing grants, verifying receipts, and signing download URLs. It runs on Node.js 20 or later. Any language can implement the same JWT and HMAC contracts.

### 1. Issue an upload grant

After your backend authenticates the user and checks permission to modify the resource:

```js
import { createGrant, emailHash } from './client.mjs';

const uploadToken = createGrant({
  key: process.env.IMGSERVER_SIGNING_KEY,
  issuer: 'auth',
  subject: currentUser.id,
  tenant: currentUser.tenantId,
  resource: `user:${currentUser.id}`,
  profile: 'avatar',
  alias: emailHash(currentUser.email),
});
// Return uploadToken and the imgserver URL to the browser.
```

For a project avatar, use the same `profile: 'avatar'` and upload endpoint, with `alias: avatarHash('project:' + project.id)`. Import `avatarHash` from `client.mjs`; it hashes the exact string supplied. The project backend can set `issuer: 'projects'` and `resource: 'project:' + project.id` to bind authorization to the project. Processing, sizes and download routes are identical to user avatars.

Use `issuer: 'support'`, `resource: 'ticket:42'`, and `profile: 'ticket'` for a ticket attachment. Resource values are opaque to imgserver; your backend defines their meaning and permissions.

All backends holding the shared key have the same trust level and can issue any grant. `iss` identifies a namespace; it does not isolate mutually untrusted backends. Give the browser only the scoped JWT, never the signing key or S3 credentials.

### 2. Upload from the browser

```js
const body = new FormData();
body.append('file', selectedFile);
const response = await fetch(`${mediaURL}/v1/uploads`, {
  method: 'POST',
  headers: { Authorization: `Bearer ${uploadToken}` },
  body,
});
if (!response.ok) throw new Error(await response.text());
const { file, urls, receipt } = await response.json();
// Submit receipt to your backend to attach file.id to the domain resource.
```

Send exactly one multipart field named `file`. Do not set `Content-Type` yourself in the browser; FormData supplies the boundary. The upload passes through imgserver to S3. No S3 integration is required in the application backends or browser.

The response looks like:

```json
{
  "file": {
    "id": "opaque-random-id",
    "issuer": "auth",
    "tenant": "acme",
    "subject": "user-123",
    "resource": "user:user-123",
    "profile": "avatar",
    "alias": "b58996c504c5638798eb6b511e6f49af",
    "public": true,
    "content_type": "image/png",
    "size": 12345,
    "sha256": "sha256-of-normalized-image"
  },
  "urls": {
    "content": "https://media.example.com/v1/files/opaque-random-id/content",
    "alias": "https://media.example.com/avatar/b58996c504c5638798eb6b511e6f49af",
    "128": "https://media.example.com/v1/files/opaque-random-id/content?size=128"
  },
  "receipt": "signed-receipt-jwt"
}
```

The full response also contains name, timestamps, dimensions, and variant metadata. Private file URLs need authorization; they are not public merely because the API returns a URL.

### 3. Verify and attach the receipt

```js
import { verifyReceipt } from './client.mjs';

const verified = verifyReceipt(request.body.receipt, {
  key: process.env.IMGSERVER_SIGNING_KEY,
  issuer: 'auth',
  subject: currentUser.id,
  tenant: currentUser.tenantId,
  resource: `user:${currentUser.id}`,
  profile: 'avatar',
});
await saveAvatarFileId(currentUser.id, verified.file_id);
```

Compute expected scope from your authenticated session and resource, rather than accepting those fields from the browser. Store the stable file ID. Receipts expire after one hour and are evidence of an upload, not download credentials. A receipt can be submitted more than once; your backend should make the attachment operation idempotent. If a file may have been deleted since upload, verify its current status with the metadata endpoint before attaching it.

## JWT contract

Backend grants use HS256 with the shared `signing_key` as the raw UTF-8 key, `aud: "imgserver"`, and the following claims:

| Claim | Meaning |
| --- | --- |
| `iss` | Backend namespace, such as `auth`, `support`, or `projects` |
| `sub` | Acting user ID |
| `iat`, `exp` | Issued/expiry Unix timestamps; maximum lifetime 15 minutes |
| `jti` | Unique grant ID; required for replay handling |
| `action` | `upload`, `read`, or `delete` |
| `tenant` | Tenant namespace |
| `resource` | Authorized domain resource |
| `profile` | Required for upload |
| `alias` | Optional public lookup alias, if profile permits it |
| `max_bytes` | Optional tighter upload size limit |
| `file_id` | Required for read/delete |

Issuer names, tenants, profiles and aliases use ASCII letters, digits, `_` and `-`, start with a letter/digit, and are at most 128 characters. Avatar aliases must instead be exactly 32 lowercase hexadecimal characters. Resource names may contain punctuation such as `:` and are at most 256 bytes. Clocks should be synchronized; expired/future-issued tokens are rejected without leeway.

Upload grants are single-use capabilities with retry support. Repeating a completed grant returns the same file ID with HTTP 200, without replacing bytes. Concurrent requests with the same grant return 409. Reusing the same `iss`/`jti` with different claims returns 409. Ordinary failures release the reservation for retry; after a process crash, issue a new grant. Files always receive random, immutable IDs and object keys.

Receipts use `iss: "imgserver"`, `aud: <backend issuer>`, `sub: <acting user>`, `jti: <file ID>`, `iat`, `exp`, plus `file_id`, `tenant`, `resource`, `profile`, optional `alias`, `sha256`, and `size`. Their HS256 signing key is the raw 32-byte output of:

```text
HMAC-SHA256(shared_key, "imgserver-v1:receipt")
```

Receipt/download subkeys are derived internally; there is still only **one configured key**. This prevents accidentally accepting a receipt as a backend grant.

## Public aliases

```text
GET /avatar/<md5>
GET /avatar/<md5>?size=128
GET /v1/public/<profile>/<alias>?size=128
```

User and project avatars share one `avatar` profile, upload mechanism and `/avatar/<md5>` namespace. Only the identifier being hashed changes:

```js
import { avatarHash, emailHash } from './client.mjs';

const userKey = emailHash('user@example.com');
const projectKey = avatarHash('project:550e8400-e29b-41d4-a716-446655440000');
// For either: upload with profile: 'avatar', alias: key.
// Fetch through /avatar/<key> or /avatar/<key>?size=128.
```

For email avatars, use `MD5(lowercase(trim(email)))`, encoded as lowercase hexadecimal. For project avatars, use the exact UTF-8 string `project:<uuid>` as the MD5 input. The service receives the hash and does not distinguish user avatars from project avatars. Hashes provide lookup compatibility, not privacy.

If a valid MD5 has no uploaded avatar, the service returns a deterministic PNG identicon: a colored, mirrored pattern derived from the hash. It is generated locally without database writes or S3 access and works for both user and project keys. The default image is 256×256; `?size=128` and other sizes configured on the avatar profile are supported. Uploading an avatar replaces the identicon automatically; deleting the avatar restores it. Invalid hashes still return 404, unavailable sizes return 400, and database failures return 503.

Generated avatars support HEAD and ETags and use the same 60-second cache policy as uploaded avatar aliases, so an upload becomes visible after the cache expires or revalidation occurs. The same fallback is available through `/v1/public/avatar/<md5>`. Other public profiles keep their usual missing-file behavior, and disabling public access to the avatar profile disables the fallback too.

Aliases are globally unique per profile; use globally unique project UUIDs in `project:<uuid>`. Uploading again with the same alias, issuer, tenant and resource atomically updates the alias after the upload succeeds. Previous file IDs remain valid until explicitly deleted, so other references are not broken. Aliases retain their ownership even after deletion to prevent takeover; deleting an old file does not affect a newer replacement.

The service supports ETags and HEAD. Public file content caches for one hour, mutable aliases for 60 seconds, and private responses use `Cache-Control: no-store`. Deleted public bytes can remain in browser/CDN caches until expiry. Configure CDN purging separately if immediate public removal is required.

Configuration changes take effect on restart. Making a profile private revokes public access to its existing uploads at the origin. Making a profile public does not publish files originally uploaded privately.

## Private downloads

Your support backend checks that a user can access the ticket, then either creates a `read` grant or directly signs a URL using the helper:

```js
import { signDownloadURL } from './client.mjs';

const url = signDownloadURL({
  key: process.env.IMGSERVER_SIGNING_KEY,
  baseURL: 'https://media.example.com',
  fileId: attachment.fileId,
  ttlSeconds: 300,
  // size: 128, // only for an existing configured image variant
});
```

Alternatively:

```http
POST /v1/files/<id>/download-url
Authorization: Bearer <read-grant>
Content-Type: application/json

{"ttl_seconds": 300}
```

Signed URLs expire after at most 15 minutes when issued through the endpoint/helper. Anyone holding a URL can use it until expiry; deleting the file invalidates it at the origin. A valid scoped `read` bearer token can also access content directly. Read/delete grants must match the stored file ID, issuer, tenant and resource. The acting user may differ because a ticket can be shared by multiple users; the backend decides who is allowed to receive a grant.

For other languages, the download signature is unpadded base64url of:

```text
download_key = HMAC-SHA256(shared_key, "imgserver-v1:download")
signature = HMAC-SHA256(download_key,
  "imgserver-download-v1\n" + file_id + "\n" + variant + "\n" + expires)
```

`variant` is `original` or the canonical decimal size, and `expires` is a decimal Unix timestamp. There is no trailing newline. Use `/v1/files/<id>/content?expires=<expires>&signature=<signature>` and add `&size=<variant>` for image variants. Signatures are bound to both file ID and size. The verifier accepts a correctly signed future expiry; the signer is responsible for enforcing the 15-minute policy. Keep signed URLs out of request logs.

## HTTP API

| Method | Path | Authorization |
| --- | --- | --- |
| POST | `/v1/uploads` | Upload grant |
| GET | `/v1/files/{id}` | Read grant, including for public metadata |
| GET / HEAD | `/v1/files/{id}/content` | Public, read grant, or signed URL |
| POST | `/v1/files/{id}/download-url` | Read grant |
| DELETE | `/v1/files/{id}` | Delete grant |
| GET / HEAD | `/avatar/{alias}` | Public |
| GET / HEAD | `/v1/public/{profile}/{alias}` | Public |
| GET | `/healthz`, `/readyz` | Public |
| GET | `/openapi.json`, `/docs` | Public |

All error bodies are JSON with an `error` string. A successful delete returns 204. Deletion hides metadata/content first, then removes all objects; retry a 503 to finish storage cleanup. The persistent cleanup worker also retries pending deletions every five minutes. Interrupted uploads have their object keys recorded before writing; after one hour, abandoned objects are eligible for cleanup. Workers process up to 100 cleanup records per pass. SQLite metadata and the cleanup queue must be persisted together.

## S3 configuration

For S3, copy `config.example.json` to `config.json`, set `IMGSERVER_SIGNING_KEY` to a random value of at least 32 bytes, and configure your bucket/region. Use the same key in the application backends. For a strong random value, `openssl rand -hex 32` is sufficient.

```json
"storage": {
  "driver": "s3",
  "bucket": "my-private-media",
  "region": "us-east-1",
  "endpoint": "",
  "path_style": false
}
```

For an S3-compatible provider, set its endpoint and region and, when required, `path_style: true`. For local storage, use `{"driver":"local","directory":".data/objects"}`.

The service uses the AWS SDK default credential chain: environment credentials, shared AWS configuration, or an IAM role. Keep the entire bucket private, including objects served publicly through imgserver. All access policy enforcement happens in this service; no object ACLs are used. Required IAM permissions are `s3:GetObject`, `s3:PutObject`, and `s3:DeleteObject` on `arn:aws:s3:::<bucket>/files/*`, plus `s3:ListBucket` on the bucket for readiness checks. The [AWS SDK S3 documentation](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/s3) documents custom endpoints and S3 operations.

Use a dedicated bucket or prefix. If bucket versioning is enabled, S3 deletion creates delete markers; apply a noncurrent-version retention policy appropriate to your data. Back up both metadata and objects. SQLite is intended for a single service instance with a persistent local volume; horizontal deployment needs a shared database implementation.

`IMGSERVER_BASE_URL` and `IMGSERVER_LISTEN` override the matching config fields. Configure the public origin without a path prefix. `allowed_origins` lists exact browser origins for CORS, with no trailing slash. HTTPS termination belongs at your reverse proxy. Bound request sizes/timeouts there to your intended deployment limits.

## Docker

The image runs as a non-root user with a healthcheck, a read-only filesystem, a persistent data volume, and temporary disk for uploads. The container listens on port 18080. Configuration and credentials are supplied at runtime.

Build and run from source after preparing an env-based Docker configuration:

```sh
docker compose up --build -d
```

Use `config.local.example.json` for local storage or `config.example.json` for S3, and set the shared key in `.env`. The JSON examples contain an environment-variable reference instead of a secret. The bind-mounted JSON must be readable by container UID 65532; keep the secret in the private `.env` file. Preserve your existing signing key when changing deployment methods.

See [DISTRIBUTION.md](DISTRIBUTION.md) for complete setup, prebuilt GHCR images, version tags, release publishing through GitHub Actions, updates, and rollback instructions. The workflow tests pull requests and publishes images for Linux AMD64 and ARM64 after its checks pass.

## CLI examples

Issue a private attachment grant:

```sh
go run . token -issuer support -subject user-1 -tenant acme \
  -resource ticket:42 -profile ticket
```

Upload with that token:

```sh
curl -H 'Authorization: Bearer <upload-token>' \
  -F 'file=@attachment.pdf' http://localhost:18080/v1/uploads
```

Issue a read or delete grant:

```sh
go run . token -issuer support -tenant acme -resource ticket:42 \
  -action read -file '<returned-file-id>'
go run . token -issuer support -tenant acme -resource ticket:42 \
  -action delete -file '<returned-file-id>'
```

Verify an upload receipt locally:

```sh
go run . verify-receipt -issuer support -receipt '<receipt-jwt>'
```

The CLI verifies the receipt signature, issuer, audience and expiry and prints claims; your application must additionally match the expected upload scope as the helper does.

## Validation and current boundaries

```sh
go test -race ./...
go vet ./...
node --test client.test.mjs
```

Tests cover public/private flows, image variants, single-use grants and concurrent retries, alias ownership, JWT validation, signed URL tampering/expiry, persistent metadata and cleanup, and real AWS SDK requests to a local S3 protocol fixture. They do not require an AWS account. That fixture exercises signed HTTP PUT/GET/DELETE/HEAD calls; test your provider's endpoint and credentials before deployment.

This version uploads through imgserver and streams downloads through it. Direct-to-S3 presigned uploads, resumable/multipart uploads, HTTP range downloads, content scanning, and automatic deletion of unreferenced completed uploads are outside this version. Your backends should delete files that are no longer referenced. Arbitrary attachments are served with attachment disposition; normalized image profiles can render inline.
