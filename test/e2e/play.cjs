// End-to-end player check against the stock frontend.
// Usage: node play.cjs <url> [chrome|firefox] [seconds] [layers]
// Reports: video size/frames, data channel open, UE->player messages, a latency-test round trip
// (player->streamer->player over the data channel), CSP violations and page errors.
// With "layers", steps the frontend's preferred quality through each SFU layer and reports sizes.
const { chromium, firefox } = require('playwright');
const [url, which = 'chrome', secs = '20', mode = ''] = process.argv.slice(2);
(async () => {
    const insecure = !!process.env.INSECURE;
    const browser = which === 'firefox'
        ? await firefox.launch({ firefoxUserPrefs: { 'media.autoplay.default': 0, 'media.autoplay.blocking_policy': 0 } })
        : await chromium.launch({ channel: 'chrome', args: ['--autoplay-policy=no-user-gesture-required'] });
    const page = await (await browser.newContext({ ignoreHTTPSErrors: insecure })).newPage();
    const problems = [];
    page.on('console', (m) => { const t = m.text(); if (/Content.Security.Policy|Refused to/i.test(t)) problems.push('CSP: ' + t.slice(0, 200)); });
    page.on('pageerror', (e) => problems.push('pageerror: ' + e.message.slice(0, 200)));
    await page.goto(url);
    await page.waitForFunction(() => window.pixelStreaming, null, { timeout: 15000 });
    await page.evaluate(() => {
        window.__r = { initialSettings: 0, latency: 0, dataChannelOpen: 0, qp: 0, webRtcConnected: 0 };
        const ps = window.pixelStreaming;
        ps.addEventListener('initialSettings', () => window.__r.initialSettings++);
        ps.addEventListener('latencyTestResult', () => window.__r.latency++);
        ps.addEventListener('dataChannelOpen', () => window.__r.dataChannelOpen++);
        ps.addEventListener('videoEncoderAvgQP', () => window.__r.qp++);
        ps.addEventListener('webRtcConnected', () => window.__r.webRtcConnected++);
    });
    const sample = () => page.evaluate(() => {
        const v = document.querySelector('video');
        if (!v) return null;
        const q = v.getVideoPlaybackQuality ? v.getVideoPlaybackQuality() : {};
        return { w: v.videoWidth, h: v.videoHeight, frames: q.totalVideoFrames, t: v.currentTime.toFixed(1) };
    });
    const half = Number(secs) * 500;
    await new Promise((r) => setTimeout(r, half));
    const first = await sample();
    await page.evaluate(() => window.pixelStreaming.requestLatencyTest());
    if (mode === 'layers') {
        // The stock UI's Preferred Quality setting; the frontend turns it into layerPreference.
        const options = await page.evaluate(() => window.pixelStreaming.config.getSettingOption('PreferredQuality').options);
        console.log('quality options:', JSON.stringify(options));
        for (const q of [...options].reverse().concat(options[0])) {
            await page.evaluate((q) => window.pixelStreaming.config.setOptionSettingValue('PreferredQuality', q), q);
            await new Promise((r) => setTimeout(r, 4000));
            console.log(`${q}:`, JSON.stringify(await sample()));
        }
    }
    await new Promise((r) => setTimeout(r, half));
    const last = await sample();
    const r = await page.evaluate(() => window.__r);
    console.log(JSON.stringify({ browser: which, first, last, events: r, problems }));
    await browser.close();
    const ok = last && last.w > 0 && (last.frames > (first?.frames ?? 0) || Number(last.t) > Number(first?.t ?? 0) + 2) && r.dataChannelOpen > 0 && r.latency > 0;
    console.log(ok ? 'PASS' : 'FAIL');
    process.exit(ok ? 0 : 1);
})().catch((e) => { console.error('ERROR', e.message); process.exit(2); });
