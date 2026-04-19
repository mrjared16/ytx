package jsengine

const BrowserStubsJS = `
var _yt_player = {};
var _exposed = {};

// Make 'this' (the global object) behave like 'window'
this.window = this;
this.self = this;
this.globalThis = this;
this.top = this;
this.parent = this;
this.t = this;

// Attach browser properties to the global object
this.location = {
    hash: '',
    host: 'www.youtube.com',
    hostname: 'www.youtube.com',
    href: 'https://www.youtube.com/watch?v=ytx',
    origin: 'https://www.youtube.com',
    password: '',
    pathname: '/watch',
    port: '',
    protocol: 'https:',
    search: '?v=ytx',
    username: ''
};
this.document = {
    getElementsByTagName: function() { return []; },
    querySelector: function() { return null; },
    getElementById: function() { return null; },
    createElement: function(tag) {
        return {
            style: {},
            tagName: tag.toUpperCase(),
            appendChild: function() {},
            setAttribute: function() {},
            getAttribute: function() { return null; }
        };
    },
    createTextNode: function() { return {}; },
    documentElement: { style: {} },
    body: { appendChild: function() {} },
    head: { appendChild: function() {} },
    scripts: [{ src: 'https://www.youtube.com/s/player/placeholder/base.js' }],
    currentScript: { src: 'https://www.youtube.com/s/player/placeholder/base.js' },
    cookie: '',
    domain: 'youtube.com',
    location: this.location
};
this.navigator = { userAgent: 'Mozilla/5.0', platform: 'Win32', language: 'en-US', languages: ['en-US'] };
this.console = { log: function() {}, warn: function() {}, error: function() {}, info: function() {}, debug: function() {} };

var g = this.g || {};
this.g = g;
g.qJ = function(url) {
    var raw = typeof url === 'string' ? url : '';
    this.base = raw.split('?')[0] || '';
    this.params = {};
    var query = '';
    var qIdx = raw.indexOf('?');
    if (qIdx >= 0 && qIdx + 1 < raw.length) {
        query = raw.slice(qIdx + 1);
    }
    if (query) {
        var pairs = query.split('&');
        for (var i = 0; i < pairs.length; i++) {
            if (!pairs[i]) continue;
            var eq = pairs[i].indexOf('=');
            if (eq < 0) {
                this.params[pairs[i]] = '';
            } else {
                this.params[pairs[i].slice(0, eq)] = pairs[i].slice(eq + 1);
            }
        }
    }
};
g.qJ.prototype.set = function(k, v) {
    this.params[String(k)] = v == null ? '' : String(v);
    return this;
};
g.qJ.prototype.get = function(k) {
    var key = String(k);
    return Object.prototype.hasOwnProperty.call(this.params, key) ? this.params[key] : null;
};
g.qJ.prototype.clone = function() {
    return new g.qJ(this.toString());
};
g.qJ.prototype.toString = function() {
    var out = [];
    for (var key in this.params) {
        if (Object.prototype.hasOwnProperty.call(this.params, key)) {
            out.push(key + '=' + this.params[key]);
        }
    }
    return out.length ? this.base + '?' + out.join('&') : this.base;
};
var __qjMethodNames = ['append','update','setParam','add','put','setValue','setQuery'];
for (var __i = 0; __i < __qjMethodNames.length; __i++) {
    (function(name){
        g.qJ.prototype[name] = function(k, v) { return this.set(k, v); };
    })(__qjMethodNames[__i]);
}

function setTimeout(fn) { try { fn(); } catch(e) {} return 0; }
function setInterval() { return 0; }
function clearTimeout() {}
function clearInterval() {}
function requestAnimationFrame(fn) { try { fn(0); } catch(e) {} return 0; }
function cancelAnimationFrame() {}

function XMLHttpRequest() {
    this.readyState = 0; this.status = 0; this.responseText = '';
    this.open = function() { this.readyState = 1; };
    this.send = function() { this.readyState = 4; this.status = 200; };
    this.setRequestHeader = function() {};
    this.getResponseHeader = function() { return null; };
}

function fetch() {
    return Promise.resolve({
        ok: true,
        status: 200,
        json: function() { return Promise.resolve({}); },
        text: function() { return Promise.resolve(''); }
    });
}

this.localStorage = { getItem: function() { return null; }, setItem: function() {}, removeItem: function() {}, clear: function() {} };
this.sessionStorage = { getItem: function() { return null; }, setItem: function() {}, removeItem: function() {}, clear: function() {} };
this.performance = { now: function() { return Date.now(); }, timing: { navigationStart: Date.now() } };
this.history = { pushState: function() {}, replaceState: function() {} };
this.screen = { width: 1920, height: 1080 };
this.innerWidth = 1920;
this.innerHeight = 1080;
this.devicePixelRatio = 1;
this.crypto = {
    getRandomValues: function(arr) {
        for (var i = 0; i < arr.length; i++) arr[i] = Math.floor(Math.random() * 256);
        return arr;
    }
};
`
