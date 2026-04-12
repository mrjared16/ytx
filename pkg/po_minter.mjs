import { BG, buildURL, GOOG_API_KEY, USER_AGENT } from 'bgutils-js';
import { JSDOM } from 'jsdom';

const input = await Bun.stdin.text();
let req;
try {
  req = JSON.parse(input);
} catch (err) {
  console.log(JSON.stringify({ error: `invalid input: ${err.message}` }));
  process.exit(0);
}

try {
  if (!req?.videoId) throw new Error('missing videoId');
  if (!req?.visitorData) throw new Error('missing visitorData');

  const dom = new JSDOM('<!doctype html><html><head><title></title></head><body></body></html>', {
    url: 'https://www.youtube.com/',
    referrer: 'https://www.youtube.com/',
    userAgent: USER_AGENT,
  });

  Object.assign(globalThis, {
    window: dom.window,
    document: dom.window.document,
    location: dom.window.location,
    origin: dom.window.origin,
  });

  if (!Reflect.has(globalThis, 'navigator')) {
    Object.defineProperty(globalThis, 'navigator', { value: dom.window.navigator });
  }

  const requestKey = 'O43z0dpjhgX20SCx4KAo';
  let bgChallenge = null;
  if (req?.challenge) {
    const direct = req.challenge;
    if (direct && typeof direct.program === 'string' && typeof direct.globalName === 'string') {
      bgChallenge = direct;
    } else {
      bgChallenge = BG.Challenge.parseChallengeData(req.challenge);
    }
  }
  const hasInterpreter = !!(bgChallenge?.interpreterJavascript?.privateDoNotAccessOrElseSafeScriptWrappedValue || bgChallenge?.interpreterJavascript?.privateDoNotAccessOrElseTrustedResourceUrlWrappedValue);
  if (!bgChallenge || !hasInterpreter) {
    const challengeResp = await fetch(buildURL('Create', true), {
      method: 'POST',
      headers: {
        'content-type': 'application/json+protobuf',
        'x-goog-api-key': GOOG_API_KEY,
        'x-user-agent': 'grpc-web-javascript/0.1',
        'user-agent': USER_AGENT,
      },
      body: JSON.stringify([requestKey]),
    });
    if (!challengeResp.ok) throw new Error(`Create challenge failed: ${challengeResp.status}`);
    bgChallenge = BG.Challenge.parseChallengeData(await challengeResp.json());
  }
  if (!bgChallenge) {
    throw new Error('Could not parse challenge data');
  }

  let interpreterJavascript = bgChallenge.interpreterJavascript?.privateDoNotAccessOrElseSafeScriptWrappedValue || '';
  if (!interpreterJavascript && bgChallenge.interpreterJavascript?.privateDoNotAccessOrElseTrustedResourceUrlWrappedValue) {
    const interpreterURL = bgChallenge.interpreterJavascript.privateDoNotAccessOrElseTrustedResourceUrlWrappedValue;
    const fullURL = interpreterURL.startsWith('http') ? interpreterURL : `https:${interpreterURL}`;
    const interpreterResp = await fetch(fullURL);
    if (!interpreterResp.ok) {
      throw new Error(`interpreter fetch failed: ${interpreterResp.status}`);
    }
    interpreterJavascript = await interpreterResp.text();
  }

  if (!interpreterJavascript) {
    throw new Error('Could not load interpreter JS');
  }

  new Function(interpreterJavascript)();

  const botguard = await BG.BotGuardClient.create({
    globalName: bgChallenge.globalName,
    globalObj: globalThis,
    program: bgChallenge.program,
  });

  const webPoSignalOutput = [];
  const botguardResponse = await botguard.snapshot({ webPoSignalOutput });

  const integrityTokenResponse = await fetch(buildURL('GenerateIT', true), {
    method: 'POST',
    headers: {
      'content-type': 'application/json+protobuf',
      'x-goog-api-key': GOOG_API_KEY,
      'x-user-agent': 'grpc-web-javascript/0.1',
      'user-agent': USER_AGENT,
    },
    body: JSON.stringify([requestKey, botguardResponse]),
  });

  if (!integrityTokenResponse.ok) {
    throw new Error(`GenerateIT failed: ${integrityTokenResponse.status}`);
  }

  const response = await integrityTokenResponse.json();
  const integrityToken = typeof response?.[0] === 'string' ? response[0] : '';
  const fallbackToken = typeof response?.[3] === 'string' ? response[3] : '';
  const tokenForMinter = integrityToken || fallbackToken;
  if (!tokenForMinter) {
    throw new Error(`GenerateIT returned no token: ${JSON.stringify(response).slice(0, 300)}`);
  }

  const minter = await BG.WebPoMinter.create({ integrityToken: tokenForMinter }, webPoSignalOutput);
  const playerToken = await minter.mintAsWebsafeString(req.videoId);
  const urlToken = await minter.mintAsWebsafeString(req.visitorData);

  const ttlSeconds = Number.isFinite(response?.[1]) ? response[1] : 0;
  console.log(JSON.stringify({ playerToken, urlToken, ttlSeconds }));
} catch (err) {
  console.log(JSON.stringify({ error: String(err?.message || err) }));
}
