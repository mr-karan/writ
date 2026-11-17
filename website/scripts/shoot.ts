// Takes screenshots with a headless Chromium, driven over the DevTools
// protocol so the site needs no browser library. Run as a script, it reads
// a JSON list of shots from the file named by its first argument:
//
//   [{ "url": "http://127.0.0.1:4318/", "out": "home-1440-light.png",
//      "width": 1440, "height": 900, "scheme": "light", "dpr": 1,
//      "full": true, "maxHeight": 1600, "clip": "#plan-approvals",
//      "eval": "document.documentElement.scrollHeight" }]
//
// `full` shoots the whole page, up to `maxHeight` CSS pixels. `clip` shoots
// the box of the first element that matches a selector. `eval` prints the
// value of an expression, after the page loads, as one JSON line. A shot
// with no `out` only evaluates. Every shot runs in one browser, so a
// cookie that one sets is sent by the next. CHROMIUM names the browser.

import { spawn } from 'node:child_process';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

export interface Shot {
  url: string;
  out?: string;
  width: number;
  height: number;
  scheme?: 'light' | 'dark';
  dpr?: number;
  full?: boolean;
  maxHeight?: number;
  clip?: string;
  eval?: string;
  javascript?: boolean;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export async function shoot(shots: Shot[]): Promise<void> {
  const profile = await mkdtemp(join(tmpdir(), 'writ-shoot-'));
  const browser = spawn(
    process.env.CHROMIUM ?? 'chromium',
    ['--headless=new', '--remote-debugging-port=0', `--user-data-dir=${profile}`, '--no-first-run', '--hide-scrollbars', '--disable-gpu', 'about:blank'],
    { stdio: ['ignore', 'ignore', 'pipe'] },
  );

  const wsUrl = await new Promise<string>((resolve, reject) => {
    let err = '';
    browser.stderr.on('data', (chunk: Buffer) => {
      err += chunk.toString();
      const m = /DevTools listening on (ws:\/\/\S+)/.exec(err);
      if (m) resolve(m[1]);
    });
    browser.on('exit', (code) => reject(new Error(`chromium exited with ${code}: ${err}`)));
  });

  const ws = new WebSocket(wsUrl);
  await new Promise((resolve) => ws.addEventListener('open', resolve, { once: true }));
  let next = 0;
  const waiting = new Map<number, { resolve: (v: any) => void; reject: (e: Error) => void }>();
  const listeners: ((msg: any) => void)[] = [];
  ws.addEventListener('message', (event) => {
    const msg = JSON.parse(String(event.data));
    if (msg.id !== undefined) {
      const w = waiting.get(msg.id);
      waiting.delete(msg.id);
      if (msg.error) w?.reject(new Error(`${msg.error.message}`));
      else w?.resolve(msg.result);
      return;
    }
    for (const l of listeners) l(msg);
  });

  function send(method: string, params: object = {}, sessionId?: string): Promise<any> {
    const id = ++next;
    ws.send(JSON.stringify({ id, method, params, sessionId }));
    return new Promise((resolve, reject) => waiting.set(id, { resolve, reject }));
  }

  function loaded(sessionId: string): Promise<void> {
    return new Promise((resolve) => {
      const l = (msg: any) => {
        if (msg.sessionId === sessionId && msg.method === 'Page.loadEventFired') {
          listeners.splice(listeners.indexOf(l), 1);
          resolve();
        }
      };
      listeners.push(l);
    });
  }

  async function evaluate(sessionId: string, expression: string): Promise<any> {
    const r = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true }, sessionId);
    if (r.exceptionDetails) throw new Error(`${expression}: ${r.exceptionDetails.text}`);
    return r.result.value;
  }

  try {
    for (const shot of shots) {
      const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
      const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
      await send('Page.enable', {}, sessionId);
      await send('Emulation.setDeviceMetricsOverride', { width: shot.width, height: shot.height, deviceScaleFactor: shot.dpr ?? 1, mobile: shot.width < 600 }, sessionId);
      await send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: shot.scheme ?? 'light' }, { name: 'prefers-reduced-motion', value: 'reduce' }] }, sessionId);
      if (shot.javascript === false) await send('Emulation.setScriptExecutionDisabled', { value: true }, sessionId);
      const done = loaded(sessionId);
      await send('Page.navigate', { url: shot.url }, sessionId);
      await done;
      // Waits run here, not in the page, where a timer never fires without script.
      await evaluate(sessionId, 'document.fonts.ready.then(() => true)');
      await sleep(400);
      if (shot.eval) console.log(JSON.stringify({ url: shot.url, width: shot.width, scheme: shot.scheme ?? 'light', value: await evaluate(sessionId, shot.eval) }));
      if (shot.out) {
        let clip = { x: 0, y: 0, width: shot.width, height: shot.height, scale: 1 };
        if (shot.full) {
          // Scroll through once, as a reader would, so lazy images load.
          for (let y = 0; y < (await evaluate(sessionId, 'document.documentElement.scrollHeight')); y += shot.height / 2) {
            await evaluate(sessionId, `scrollTo(0, ${y})`);
            await sleep(60);
          }
          await evaluate(sessionId, 'scrollTo(0, 0)');
          await evaluate(sessionId, 'Promise.all([...document.images].filter((i) => i.offsetParent).map((i) => i.decode().catch(() => {})))');
          const h: number = await evaluate(sessionId, 'document.documentElement.scrollHeight');
          clip.height = Math.min(h, shot.maxHeight ?? h);
        }
        if (shot.clip) {
          const box = await evaluate(
            sessionId,
            `(() => { const r = document.querySelector(${JSON.stringify(shot.clip)})?.getBoundingClientRect(); return r && { x: r.x + scrollX, y: r.y + scrollY, width: r.width, height: r.height }; })()`,
          );
          if (!box) throw new Error(`${shot.url}: nothing matches ${shot.clip}`);
          clip = { ...box, scale: 1 };
        }
        const { data } = await send('Page.captureScreenshot', { format: 'png', clip, captureBeyondViewport: true }, sessionId);
        await writeFile(shot.out, Buffer.from(data, 'base64'));
        console.error(`shoot: ${shot.out}`);
      }
      await send('Target.closeTarget', { targetId });
    }
  } finally {
    ws.close();
    browser.kill();
    await rm(profile, { recursive: true, force: true });
  }
}

if (import.meta.main) await shoot(JSON.parse(await readFile(process.argv[2], 'utf8')));
