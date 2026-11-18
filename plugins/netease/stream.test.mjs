import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(new URL('./index.js', import.meta.url), 'utf8');
let instance = 0;

async function pluginFor(t, answer) {
  const fetch = globalThis.fetch;
  const host = globalThis.host;
  const store = new Map([['session', { cookies: { MUSIC_U: 'test-only' }, profile: null }]]);
  globalThis.host = {
    storage: { get: key => store.get(key), set: (key, value) => store.set(key, value), remove: key => store.delete(key) },
    crypto: { md5: () => '', aesEcbEncrypt: () => 'ab' },
  };
  globalThis.fetch = async (url, init) => {
    assert.equal(url, 'https://interfacepc.music.163.com/eapi/song/enhance/player/url/v1');
    return Response.json(await answer(init.headers));
  };
  t.after(() => { globalThis.fetch = fetch; globalThis.host = host; });
  const plugin = await import('data:text/javascript;base64,' + Buffer.from(`${source}\n// instance ${++instance}`).toString('base64'));
  return { plugin, store };
}

const audio = {
  id: 123, code: 200, url: 'https://m1.music.126.net/audio.flac', type: 'flac', level: 'lossless', size: 1234,
};

test('the desktop request identifies one client and exposes a complete FLAC', async t => {
  const { plugin } = await pluginFor(t, headers => {
    const version = /(?:^|; )appver=([^;]+)/.exec(headers.Cookie)?.[1];
    assert.ok(version);
    assert.match(headers.Cookie, /(?:^|; )os=pc(?:; |$)/);
    assert.ok(headers['User-Agent'].endsWith(`NeteaseMusicDesktop/${version}`));
    return { code: 200, data: [audio] };
  });
  const stream = await plugin.stream({ id: '123', quality: 'lossless' });
  assert.deepEqual([stream.url, stream.type, stream.quality, stream.size], [audio.url, 'flac', 'lossless', 1234]);
  assert.deepEqual(Object.keys(stream.headers).sort(), ['Referer', 'User-Agent']);
});

test('lossless requests reject previews, wrong variants and non-CDN URLs', async t => {
  let item;
  const { plugin } = await pluginFor(t, () => ({ code: 200, data: [item] }));
  for (const invalid of [
    { freeTrialInfo: {} }, { type: 'mp3' }, { level: 'exhigh' },
    { url: 'https://m1.music.126.net.evil.test/audio.flac' },
  ]) {
    item = { ...audio, ...invalid };
    await assert.rejects(plugin.stream({ id: '123', quality: 'lossless' }));
  }
});

test('content refusal keeps the current login and never tries another quality', async t => {
  let calls = 0;
  const { plugin, store } = await pluginFor(t, () => { calls++; return { code: 401 }; });
  await assert.rejects(plugin.stream({ id: '123', quality: 'lossless' }), { code: 'unavailable' });
  assert.equal(calls, 1);
  assert.equal(store.get('session').cookies.MUSIC_U, 'test-only');
});

test('coalesced playback requests are invalidated when the user signs out', async t => {
  let complete;
  let calls = 0;
  const response = new Promise(resolve => { complete = resolve; });
  const { plugin } = await pluginFor(t, () => { calls++; return response; });
  const first = plugin.stream({ id: '123', quality: 'lossless' });
  const second = plugin.stream({ id: '123', quality: 'lossless' });
  plugin.signOut();
  complete({ code: 200, data: [audio] });
  await Promise.all([assert.rejects(first, /账号状态已改变/), assert.rejects(second, /账号状态已改变/)]);
  assert.equal(calls, 1);
});
