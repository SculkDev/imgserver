// Optional backend helper. Node.js 20+, no npm dependencies. Never import in browsers.
import { createHmac, createHash, randomUUID, timingSafeEqual } from 'node:crypto';

function checkKey(key) {
  if (typeof key !== 'string' || Buffer.byteLength(key) < 32) throw new Error('A signing key of at least 32 bytes is required');
}
const hmac = (key, text) => createHmac('sha256', key).update(text).digest();
const derivedKey = (key, purpose) => hmac(key, `imgserver-v1:${purpose}`);
const b64json = value => Buffer.from(JSON.stringify(value)).toString('base64url');
const nowSeconds = () => Math.floor(Date.now() / 1000);

// Hash any canonical avatar identifier, including project:<uuid>, exactly as supplied.
export function avatarHash(key) {
  if (typeof key !== 'string' || !key.length) throw new Error('An avatar identifier is required');
  return createHash('md5').update(key, 'utf8').digest('hex');
}

export function emailHash(email) {
  return avatarHash(email.trim().toLowerCase());
}

export function createGrant({ key, issuer, subject, tenant, resource, action = 'upload', profile, alias, fileId, maxBytes, ttlSeconds = 300 }) {
  checkKey(key);
  if (![issuer, subject, tenant, resource].every(v => typeof v === 'string' && v.length)) throw new Error('issuer, subject, tenant and resource are required');
  if (!['upload', 'read', 'delete'].includes(action)) throw new Error('Invalid action');
  if (!Number.isInteger(ttlSeconds) || ttlSeconds < 1 || ttlSeconds > 900) throw new Error('ttlSeconds must be 1..900');
  if (action === 'upload' && !profile) throw new Error('An upload profile is required');
  if (maxBytes !== undefined && (!Number.isSafeInteger(maxBytes) || maxBytes < 1)) throw new Error('maxBytes must be a positive integer');
  if (action !== 'upload' && !fileId) throw new Error('fileId is required');
  const now = nowSeconds();
  const payload = { iss: issuer, sub: subject, aud: 'imgserver', iat: now, exp: now + ttlSeconds, jti: randomUUID(), action, tenant, resource };
  if (action === 'upload') Object.assign(payload, { profile, ...(alias ? { alias } : {}), ...(maxBytes ? { max_bytes: maxBytes } : {}) });
  else payload.file_id = fileId;
  const input = `${b64json({ alg: 'HS256', typ: 'JWT' })}.${b64json(payload)}`;
  return `${input}.${hmac(key, input).toString('base64url')}`;
}

export function verifyReceipt(token, { key, issuer, tenant, resource, subject, profile }) {
  checkKey(key);
  if (![issuer, tenant, resource, subject, profile].every(v => typeof v === 'string' && v.length)) throw new Error('All expected receipt scope fields are required');
  if (typeof token !== 'string' || token.length > 8192) throw new Error('Invalid receipt');
  const parts = token.split('.');
  if (parts.length !== 3) throw new Error('Invalid receipt');
  const expected = hmac(derivedKey(key, 'receipt'), `${parts[0]}.${parts[1]}`);
  const actual = Buffer.from(parts[2], 'base64url');
  if (actual.length !== expected.length || !timingSafeEqual(actual, expected)) throw new Error('Invalid receipt signature');
  const header = JSON.parse(Buffer.from(parts[0], 'base64url'));
  const claims = JSON.parse(Buffer.from(parts[1], 'base64url'));
  const audience = Array.isArray(claims.aud) ? claims.aud : [claims.aud];
  if (header.alg !== 'HS256' || claims.iss !== 'imgserver' || !audience.includes(issuer) || !Number.isInteger(claims.exp) || claims.exp <= nowSeconds()) throw new Error('Invalid or expired receipt');
  if (claims.tenant !== tenant || claims.resource !== resource || claims.sub !== subject || claims.profile !== profile) throw new Error('Receipt scope does not match the expected upload');
  if (typeof claims.file_id !== 'string' || !claims.file_id || claims.file_id !== claims.jti) throw new Error('Invalid receipt file ID');
  return claims;
}

// Authorize the caller's access to this file in your backend before calling.
// This avoids a server round trip to the download-url endpoint.
export function signDownloadURL({ key, baseURL, fileId, size, ttlSeconds = 300 }) {
  checkKey(key);
  if (!/^[A-Za-z0-9_-]+$/.test(fileId || '')) throw new Error('Invalid fileId');
  if (!Number.isInteger(ttlSeconds) || ttlSeconds < 1 || ttlSeconds > 900) throw new Error('ttlSeconds must be 1..900');
  if (size !== undefined && (!Number.isInteger(size) || size < 16 || size > 2048)) throw new Error('Invalid image size');
  const variant = size === undefined ? 'original' : String(size);
  const expires = String(nowSeconds() + ttlSeconds);
  const signature = hmac(derivedKey(key, 'download'), `imgserver-download-v1\n${fileId}\n${variant}\n${expires}`).toString('base64url');
  const url = new URL(`/v1/files/${fileId}/content`, baseURL);
  url.searchParams.set('expires', expires);
  url.searchParams.set('signature', signature);
  if (size !== undefined) url.searchParams.set('size', String(size));
  return url.toString();
}
