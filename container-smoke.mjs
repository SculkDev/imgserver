// Run after building the image: node container-smoke.mjs imgserver:local
// Each run owns a temporary container and volume; no developer data is mounted.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';

const image = process.argv[2] || 'imgserver:local';
const suffix = randomUUID();
const container = `imgserver-smoke-${suffix}`;
const volume = `imgserver-smoke-${suffix}`;
const docker = (...args) => execFileSync('docker', args, { encoding: 'utf8', timeout: 60000 }).trim();
const configuration = '/app/.data/config.json';
try {
  const version = JSON.parse(docker('run', '--rm', image, 'version'));
  assert.equal(typeof version.version, 'string');
  docker('volume', 'create', volume);
  docker('run', '--rm', '--mount', `type=volume,src=${volume},dst=/app/.data`, image, 'init', '-config', configuration);
  docker('run', '--detach', '--name', container,
    '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
    '--tmpfs', '/tmp:rw,noexec,nosuid,size=512m,mode=1777',
    '--mount', `type=volume,src=${volume},dst=/app/.data`,
    '--publish', '127.0.0.1::18080',
    '--health-cmd', `/app/imgserver healthcheck -config ${configuration}`,
    '--health-interval', '1s', '--health-start-period', '1s',
    image, 'serve', '-config', configuration);
  const binding = JSON.parse(docker('inspect', container))[0].NetworkSettings.Ports['18080/tcp'][0];
  let origin = `http://127.0.0.1:${binding.HostPort}`;
  let ready = false;
  for (let attempt = 0; attempt < 60; attempt++) {
    try { ready = (await fetch(`${origin}/readyz`, { signal: AbortSignal.timeout(2000) })).ok; } catch {}
    if (ready) break;
    await new Promise(resolve => setTimeout(resolve, 500));
  }
  assert.ok(ready, 'Container did not become ready');
  assert.equal((await fetch(`${origin}/docs`)).status, 200);
  const spec = await (await fetch(`${origin}/openapi.json`)).json();
  assert.ok(spec.paths['/avatar/{alias}']);
  assert.equal(JSON.parse(docker('inspect', container))[0].Config.User, '65532:65532');
  assert.equal(JSON.parse(docker('inspect', container))[0].HostConfig.ReadonlyRootfs, true);
  docker('exec', container, '/app/imgserver', 'healthcheck', '-config', configuration);
  // Exercise persistent writes, restarts, and an authorized private download.
  const token = docker('exec', container, '/app/imgserver', 'token', '-config', configuration,
    '-issuer', 'support', '-tenant', 'acme', '-resource', 'ticket:42', '-profile', 'ticket');
  const data = new FormData(); data.append('file', new Blob(['persisted container upload']), 'ticket.txt');
  const response = await fetch(`${origin}/v1/uploads`, { method: 'POST', headers: { Authorization: `Bearer ${token}` }, body: data });
  assert.equal(response.status, 201); const uploaded = await response.json();
  assert.equal((await fetch(`${origin}/v1/files/${uploaded.file.id}/content`)).status, 401);
  docker('restart', '--time', '10', container);
  // Docker can allocate a new ephemeral host port when a container restarts.
  const restartedBinding = JSON.parse(docker('inspect', container))[0].NetworkSettings.Ports['18080/tcp'][0];
  origin = `http://127.0.0.1:${restartedBinding.HostPort}`;
  ready = false;
  for (let attempt = 0; attempt < 60; attempt++) {
    try { ready = (await fetch(`${origin}/readyz`, { signal: AbortSignal.timeout(2000) })).ok; } catch {}
    if (ready) break;
    await new Promise(resolve => setTimeout(resolve, 500));
  }
  assert.ok(ready, 'Container did not become ready after restart');
  const read = docker('exec', container, '/app/imgserver', 'token', '-config', configuration,
    '-issuer', 'support', '-tenant', 'acme', '-resource', 'ticket:42', '-action', 'read', '-file', uploaded.file.id);
  const content = await fetch(`${origin}/v1/files/${uploaded.file.id}/content`, { headers: { Authorization: `Bearer ${read}` } });
  assert.equal(content.status, 200); assert.equal(await content.text(), 'persisted container upload');
  console.log(`Container smoke test passed (${version.version}): non-root, read-only, healthy, docs, uploads and restart persistence`);
} catch (error) {
  try { console.error(docker('logs', container)); } catch {}
  throw error;
} finally {
  try { docker('rm', '--force', container); } catch {}
  try { docker('volume', 'rm', volume); } catch {}
}
