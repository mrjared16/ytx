// N-parameter transform runner for ytx
// Usage: node|bun this.js <player_js_path> <n_function_name>
// Receives JSON commands via stdin, returns JSON results via stdout

import { createContext, runInContext } from 'vm';
import { readFileSync } from 'fs';
import { createInterface } from 'readline';

// Complete browser-like environment for YouTube's player.js
function createBrowserContext() {
    const ctx = {
        _yt_player: {},
        _exposed: {},
        window: { location: { href: 'https://www.youtube.com/' } },
        document: {
            getElementsByTagName: () => [],
            getElementById: () => null,
            createElement: (tag) => ({ style: {}, tagName: tag.toUpperCase(), appendChild: () => {}, setAttribute: () => {}, getAttribute: () => null }),
            createTextNode: () => ({}),
            documentElement: { style: {} },
            body: { appendChild: () => {} },
            head: { appendChild: () => {} },
            cookie: '',
            domain: 'youtube.com',
        },
        navigator: { userAgent: 'Mozilla/5.0', platform: 'Win32', language: 'en-US', languages: ['en-US'] },
        location: { href: 'https://www.youtube.com/', hostname: 'www.youtube.com', protocol: 'https:', host: 'www.youtube.com', pathname: '/', search: '', hash: '' },
        console: { log: () => {}, warn: () => {}, error: () => {}, info: () => {}, debug: () => {} },
        setTimeout: (fn) => { try { fn(); } catch(e) {} return 0; },
        setInterval: () => 0,
        clearTimeout: () => {},
        clearInterval: () => {},
        requestAnimationFrame: (fn) => { try { fn(0); } catch(e) {} return 0; },
        cancelAnimationFrame: () => {},
        atob: (s) => Buffer.from(s, 'base64').toString('binary'),
        btoa: (s) => Buffer.from(s, 'binary').toString('base64'),
        XMLHttpRequest: function() {
            this.readyState = 0;
            this.status = 0;
            this.responseText = '';
            this.response = null;
            this.open = () => { this.readyState = 1; };
            this.send = () => { this.readyState = 4; this.status = 200; };
            this.setRequestHeader = () => {};
            this.getResponseHeader = () => null;
            this.getAllResponseHeaders = () => '';
        },
        fetch: () => Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}), text: () => Promise.resolve('') }),
        localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {}, clear: () => {} },
        sessionStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {}, clear: () => {} },
        performance: { now: () => Date.now(), timing: { navigationStart: Date.now() } },
        history: { pushState: () => {}, replaceState: () => {} },
        screen: { width: 1920, height: 1080 },
        innerWidth: 1920,
        innerHeight: 1080,
        devicePixelRatio: 1,
        URL, URLSearchParams,
        crypto: { getRandomValues: (arr) => { for (let i = 0; i < arr.length; i++) arr[i] = Math.floor(Math.random() * 256); return arr; } },
        TextEncoder, TextDecoder,
        Object, Array, String, Number, Boolean, Symbol, BigInt,
        Function, RegExp, Date, Math, JSON, Error, TypeError, ReferenceError,
        parseInt, parseFloat, isNaN, isFinite, undefined,
        encodeURIComponent, decodeURIComponent, encodeURI, decodeURI,
        Promise, Map, Set, WeakMap, WeakSet, Proxy, Reflect,
        ArrayBuffer, Uint8Array, Uint16Array, Uint32Array, Int8Array, Int16Array, Int32Array,
        Float32Array, Float64Array, DataView,
        Intl,
    };
    ctx.self = ctx;
    ctx.globalThis = ctx;
    ctx.top = ctx;
    ctx.parent = ctx;
    ctx.window.document = ctx.document;
    ctx.window.navigator = ctx.navigator;
    ctx.window.location = ctx.location;
    ctx.window.XMLHttpRequest = ctx.XMLHttpRequest;
    return createContext(ctx);
}

let context = null;
let nFunc = null;

function loadFunction(code, funcName, prepared = false) {
    try {
        context = createBrowserContext();
        const modifiedCode = prepared
            ? code
            : code.replace(/\}\)\(_yt_player\);\s*$/, `_exposed['${funcName}']=${funcName};})(_yt_player);`);
        runInContext(modifiedCode, context, { timeout: 30000 });

        // Loading must yield an actual function (unary n-transform and URL
        // wrapper loads behave the same); a truthy non-function exposure is a
        // load failure, never an acceptable call target.
        const exposed = context._exposed[funcName];
        if (typeof exposed !== 'function') {
            return JSON.stringify({ error: `Function ${funcName} is not a function after loading (got ${typeof exposed})` });
        }
        nFunc = exposed;
        return JSON.stringify({ success: true });
    } catch (err) {
        return JSON.stringify({ error: err.message });
    }
}

// Load function from file path (faster than receiving 1.5MB over IPC)
function loadFunctionFromFile(filePath, funcName, prepared = false) {
    try {
        const code = readFileSync(filePath, 'utf-8');
        return loadFunction(code, funcName, prepared);
    } catch (err) {
        return JSON.stringify({ error: `Failed to read file: ${err.message}` });
    }
}

function callFunction(args) {
    try {
        if (!nFunc) {
            return JSON.stringify({ error: 'No function loaded' });
        }
        const result = nFunc(...args);
        return JSON.stringify(result);
    } catch (err) {
        return JSON.stringify({ error: err.message });
    }
}

// Batch transform multiple n values at once (optimization for bulk operations)
function batchTransform(nValues) {
    try {
        if (!nFunc) {
            return JSON.stringify({ error: 'No function loaded' });
        }
        const results = nValues.map(n => {
            try {
                return { value: nFunc(n), error: null };
            } catch (err) {
                return { value: n, error: err.message };
            }
        });
        return JSON.stringify({ results });
    } catch (err) {
        return JSON.stringify({ error: err.message });
    }
}

// Read one field from a URL-wrapper result object. Missing fields (undefined
// or null) are reported as null; a getter that throws is a hard failure and
// propagates to the caller as an explicit error.
function readWrapperKey(result, key) {
    const v = result.get(key);
    if (v === undefined || v === null) {
        return null;
    }
    if (typeof v !== 'string') {
        throw new Error(`wrapper get('${key}') returned non-string value`);
    }
    return v;
}

// URL-wrapper protocol limits: individual result fields and the whole frame
// are capped here, always below the Go-side frame cap (urlResponseMaxFrame),
// so the runner never emits an over-long or unterminated line. The Go side
// keeps its cap as a defense-in-depth termination bound.
const URL_FIELD_MAX = 16 * 1024;
const URL_FRAME_MAX = 60 * 1024;

// Build a byte-bounded error frame. A length-only cap is not enough:
// JSON.stringify expands control characters to six-byte \uXXXX escapes and
// multibyte characters beyond their string length, so a message sliced to
// URL_FIELD_MAX can still serialize over the wire limit — and a frame over
// the Go-side cap terminates the runner, turning a plain JS error into a
// fatal one. Halve the message until the serialized frame fits URL_FRAME_MAX
// (kept under the Go cap with room for the trailing newline); the '...'
// marker keeps truncation visible while the message head preserves the
// original error context.
function boundedErrorFrame(msg) {
    let text = (msg && msg.message !== undefined) ? msg.message : String(msg);
    if (text.length > URL_FIELD_MAX) {
        text = text.slice(0, URL_FIELD_MAX) + '...';
    }
    let frame = JSON.stringify({ error: text });
    while (Buffer.byteLength(frame, 'utf8') > URL_FRAME_MAX) {
        text = text.slice(0, Math.floor(text.length / 2)) + '...';
        frame = JSON.stringify({ error: text });
    }
    return frame;
}

// URL-wrapper protocol call. Invokes the loaded wrapper as
// wrapper(streamURL, signatureParam, sig) where sig is encodeURIComponent'd
// exactly once (or undefined when the signature is absent). Reads get('n')
// only when the input URL carries an n parameter, get(signatureParam) only
// when a signature was passed, and decodes the returned signature exactly
// once with decodeURIComponent. get('alr') is always read (YouTube always
// provides it). Optional missing fields are omitted; a requested signature
// that the wrapper fails to return is an explicit error, as are failed
// getters and fields over the size limit.
function urlCall(streamURL, signatureParam, signature) {
    try {
        if (typeof nFunc !== 'function') {
            return JSON.stringify({ error: 'No function loaded' });
        }
        const hasSig = (typeof signature === 'string') && signature !== '';
        const third = hasSig ? encodeURIComponent(signature) : undefined;
        const result = nFunc(streamURL, signatureParam, third);
        if (result === null || typeof result !== 'object') {
            return JSON.stringify({ error: 'URL wrapper returned non-object result' });
        }
        if (typeof result.get !== 'function') {
            return JSON.stringify({ error: 'URL wrapper object lacks getter' });
        }

        let parsed;
        try {
            parsed = new URL(streamURL);
        } catch (err) {
            return JSON.stringify({ error: `Invalid stream URL: ${err.message}` });
        }

        const out = {};
        if (parsed.searchParams.has('n')) {
            const nValue = readWrapperKey(result, 'n');
            if (nValue !== null) {
                out.n = nValue;
            }
        }
        if (hasSig) {
            const sigValue = readWrapperKey(result, signatureParam);
            if (sigValue === null || sigValue === '') {
                throw new Error(`URL wrapper did not return signature under '${signatureParam}'`);
            }
            out.sig = decodeURIComponent(sigValue);
        }
        const alrValue = readWrapperKey(result, 'alr');
        if (alrValue !== null) {
            out.alr = alrValue;
        }
        for (const key of Object.keys(out)) {
            if (out[key].length > URL_FIELD_MAX) {
                throw new Error(`wrapper get('${key}') value exceeds field limit`);
            }
        }
        const frame = JSON.stringify(out);
        // Byte-bound the serialized frame: .length counts UTF-16 units, so
        // multibyte fields within the field cap can still exceed the wire cap.
        if (Buffer.byteLength(frame, 'utf8') > URL_FRAME_MAX) {
            throw new Error('url response exceeds frame limit');
        }
        return frame;
    } catch (err) {
        return boundedErrorFrame(err);
    }
}

// Handle stdin/stdout communication
const rl = createInterface({ input: process.stdin, output: process.stdout, terminal: false });

rl.on('line', (line) => {
    try {
        const data = JSON.parse(line);
        let result;
        if (data.type === 'load') {
            result = loadFunction(data.code, data.fun, Boolean(data.prepared));
        } else if (data.type === 'load_file') {
            result = loadFunctionFromFile(data.path, data.fun, Boolean(data.prepared));
        } else if (data.type === 'call') {
            result = callFunction(data.args || []);
        } else if (data.type === 'batch') {
            result = batchTransform(data.values || []);
        } else if (data.type === 'url_call') {
            result = urlCall(data.path || '', data.fun || '', (data.values && data.values.length > 0) ? data.values[0] : '');
        } else {
            result = JSON.stringify({ error: 'Unknown command' });
        }
        console.log(result);
    } catch (err) {
        console.log(JSON.stringify({ error: 'Parse error: ' + err.message }));
    }
});

rl.on('close', () => process.exit(0));
