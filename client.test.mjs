import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHmac } from 'node:crypto';
import { avatarHash, createGrant, emailHash, signDownloadURL, verifyReceipt } from './client.mjs';

const key = '0123456789abcdef0123456789abcdef';
const expected = { key, issuer: 'auth', tenant: 'acme', resource: 'user:1', subject: 'user-1', profile: 'avatar' };
const mac = (key, value) => createHmac('sha256', key).update(value).digest();
test('user and project identifiers use the same avatar key mechanism', () => {
  assert.equal(emailHash(' USER@EXAMPLE.COM '), 'b58996c504c5638798eb6b511e6f49af');
  assert.equal(avatarHash('user@example.com'), 'b58996c504c5638798eb6b511e6f49af');
  const projectKey = avatarHash('project:550e8400-e29b-41d4-a716-446655440000');
  assert.equal(projectKey, 'ff5c6a5dcbd6ac394c2d0cdf2dbbcde4');
  assert.notEqual(avatarHash('Project:550e8400-e29b-41d4-a716-446655440000'), projectKey);
  const token = createGrant({ ...expected, alias: projectKey });
  const claims = JSON.parse(Buffer.from(token.split('.')[1], 'base64url'));
  assert.equal(claims.profile, 'avatar'); assert.equal(claims.alias, projectKey);
  assert.throws(() => avatarHash(''));
});
function receipt(overrides = {}, signingKey = mac(key, 'imgserver-v1:receipt')) {
  const claims = { iss: 'imgserver', aud: ['auth'], sub: 'user-1', tenant: 'acme', resource: 'user:1', profile: 'avatar', file_id: 'file1', jti: 'file1', exp: Math.floor(Date.now() / 1000) + 60, ...overrides };
  const input = [ { alg: 'HS256', typ: 'JWT' }, claims ].map(v => Buffer.from(JSON.stringify(v)).toString('base64url')).join('.');
  return `${input}.${mac(signingKey, input).toString('base64url')}`;
}
test('creates a scoped grant with unique jti and expected signature', () => {
  const args = { ...expected, alias: emailHash(' Person@Example.com '), ttlSeconds: 300 };
  const token = createGrant(args), [h, p, signature] = token.split('.');
  const c = JSON.parse(Buffer.from(p, 'base64url'));
  assert.equal(c.aud, 'imgserver'); assert.equal(c.action, 'upload'); assert.equal(c.exp - c.iat, 300);
  assert.equal(c.resource, 'user:1'); assert.equal(c.alias, emailHash('person@example.com'));
  assert.equal(signature, mac(key, `${h}.${p}`).toString('base64url'));
  assert.notEqual(c.jti, JSON.parse(Buffer.from(createGrant(args).split('.')[1], 'base64url')).jti);
  assert.throws(() => createGrant({ ...args, ttlSeconds: 901 }));
  assert.throws(() => createGrant({ ...args, action: 'read' }));
});
test('validates receipt signature, expiry and expected upload scope', () => {
  assert.equal(verifyReceipt(receipt(), expected).file_id, 'file1');
  for (const overrides of [{ tenant: 'other' }, { resource: 'user:2' }, { sub: 'user-2' }, { profile: 'ticket' }, { aud: 'support' }, { exp: 0 }, { iss: 'auth' }, { file_id: 'other' }]) assert.throws(() => verifyReceipt(receipt(overrides), expected));
  assert.throws(() => verifyReceipt(receipt({}, key), expected));
  assert.throws(() => verifyReceipt(receipt(), { ...expected, resource: undefined }));
});
test('download signature binds exact file, size and expiry', () => {
  const url = new URL(signDownloadURL({ key, baseURL: 'https://media.example.com', fileId: 'abc', size: 128 }));
  assert.equal(url.pathname, '/v1/files/abc/content'); assert.equal(url.searchParams.get('size'), '128');
  const message = `imgserver-download-v1\nabc\n128\n${url.searchParams.get('expires')}`;
  assert.equal(url.searchParams.get('signature'), mac(mac(key, 'imgserver-v1:download'), message).toString('base64url'));
  assert.throws(() => signDownloadURL({ key, baseURL: 'https://media.example.com', fileId: '../bad' }));
});
