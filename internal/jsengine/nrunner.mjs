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

        if (!context._exposed[funcName]) {
            return JSON.stringify({ error: `Function ${funcName} not found after loading` });
        }
        nFunc = context._exposed[funcName];
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
        } else {
            result = JSON.stringify({ error: 'Unknown command' });
        }
        console.log(result);
    } catch (err) {
        console.log(JSON.stringify({ error: 'Parse error: ' + err.message }));
    }
});

rl.on('close', () => process.exit(0));
