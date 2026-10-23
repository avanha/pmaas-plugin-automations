"use strict";
// Tests for content/static/js/automations_automations.js, with a document and a request that are fakes. Run with:
// node js_test/automations_test.js (the Go test in oldbrowsers_test.go does).
var assert = require("assert");
var fs = require("fs");
var script = require("../content/static/js/automations_automations.js");

var tests = [];
function test(name, fn) { tests.push([name, fn]); }

function button(dataset, result) {
    return {disabled: false, dataset: dataset, listeners: {}, addEventListener: function (type, fn) { this.listeners[type] = fn; },
        parentNode: {querySelector: function () { return result; }}};
}

function fakePage() {
    var result = {textContent: ""};
    var run = button({id: "a1", name: "Holiday lights"}, result);
    var rearm = button({id: "a1"}, result);
    var pause = button({id: "a1", paused: "false"}, result);
    var root = {querySelectorAll: function (selector) {
        return {"button.run-button": [run], "button.rearm-button": [rearm], "button.pause-button": [pause]}[selector];
    }};

    return {result: result, run: run, rearm: rearm, pause: pause, doc: {querySelector: function () { return root; }}};
}

function automations(page, answer) {
    var posts = [];
    var reloads = [];
    var questions = [];
    var a = new script.Automations(page.doc, {
        post: function (url, body, done) { posts.push({url: url, body: body, done: done}); },
        confirm: function (message) { questions.push(message); return answer !== false; },
        reload: function () { reloads.push(1); }
    });

    return {a: a, posts: posts, reloads: reloads, questions: questions};
}

test("running asks first, and posts the id and loads the page again", function () {
    var page = fakePage();
    var t = automations(page);
    assert.strictEqual(t.a.init(), true);

    page.run.listeners.click();

    assert.deepStrictEqual(t.questions, ["Run \"Holiday lights\" now?"]);
    assert.strictEqual(t.posts[0].url, "/plugins/automations/run");
    assert.deepStrictEqual(t.posts[0].body, {ID: "a1"});
    assert.strictEqual(page.run.disabled, true);

    t.posts[0].done(null, 200, "");
    assert.strictEqual(t.reloads.length, 1);
});

test("a no to the question sends nothing", function () {
    var page = fakePage();
    var t = automations(page, false);
    t.a.init();

    page.run.listeners.click();

    assert.strictEqual(t.posts.length, 0);
    assert.strictEqual(page.run.disabled, false);
});

test("pausing asks for the opposite of what the button says", function () {
    var page = fakePage();
    var t = automations(page);
    t.a.init();

    page.pause.listeners.click();
    assert.strictEqual(t.posts[0].url, "/plugins/automations/pause");
    assert.deepStrictEqual(t.posts[0].body, {ID: "a1", Paused: true});

    page.pause.dataset.paused = "true";
    page.pause.listeners.click();
    assert.deepStrictEqual(t.posts[1].body, {ID: "a1", Paused: false});
});

test("re-enabling loads the page again only if it was armed, and otherwise says why not", function () {
    var page = fakePage();
    var t = automations(page);
    t.a.init();

    page.rearm.listeners.click();
    assert.strictEqual(t.posts[0].url, "/plugins/automations/rearm");
    t.posts[0].done(null, 200, JSON.stringify({Armed: false, Why: "it is paused"}));
    assert.strictEqual(page.result.textContent, "Not re-enabled: it is paused");
    assert.strictEqual(page.rearm.disabled, false);
    assert.strictEqual(t.reloads.length, 0);

    page.rearm.listeners.click();
    t.posts[1].done(null, 200, JSON.stringify({Armed: true}));
    assert.strictEqual(t.reloads.length, 1);

    page.rearm.listeners.click();
    t.posts[2].done(null, 200, "<html>");
    assert.strictEqual(page.result.textContent, "Could not re-enable it: the answer was not understood");
    assert.strictEqual(t.reloads.length, 1);
});

test("a refusal and no answer are said in the words of the button, and the button comes back", function () {
    var page = fakePage();
    var t = automations(page);
    t.a.init();

    page.run.listeners.click();
    t.posts[0].done(null, 500, "boom");
    assert.strictEqual(page.result.textContent, "Could not run it (500)");
    assert.strictEqual(page.run.disabled, false);

    page.pause.listeners.click();
    t.posts[1].done(new Error("the request could not be sent"));
    assert.ok(page.result.textContent.indexOf("Could not do that: ") === 0);
    assert.strictEqual(page.pause.disabled, false);

    page.rearm.listeners.click();
    t.posts[2].done(null, 403, "");
    assert.strictEqual(page.result.textContent, "Could not re-enable it (403)");
    assert.strictEqual(t.reloads.length, 0);
});

test("init says whether there was a page of automations", function () {
    var a = new script.Automations({querySelector: function () { return null; }}, {});
    assert.strictEqual(a.init(), false);
});

// The lint every script's test has: what Safari 9 does not have, looked for in the source. Pasted into each test.
test("the script uses nothing Safari 9 does not have", function () {
    var source = fs.readFileSync(__dirname + "/../content/static/js/automations_automations.js", "utf8").replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");

    [
        [/=>/, "an arrow function"], [/\bclass\s+\w/, "a class"], [/\blet\s/, "let"], [/\bconst\s/, "const"], [/`/, "a template literal"],
        [/\basync\b/, "async"], [/\bawait\b/, "await"], [/\bfetch\s*\(/, "fetch"], [/\.\.\./, "spread"], [/\?\./, "optional chaining"],
        [/\?\?/, "nullish coalescing"], [/\bObject\.(entries|values)\b/, "Object.entries or values"], [/\.padStart\b|\.padEnd\b/, "padStart"]
    ].forEach(function (pair) {
        assert.ok(!pair[0].test(source), "the script has " + pair[1]);
    });

    // forEach is only for arrays: a NodeList has none before Safari 10, which is what toArray is for.
    (source.match(/(\w+)\.forEach/g) || []).forEach(function (use) {
        assert.ok([].indexOf(use) >= 0, "forEach on " + use);
    });
});

var failed = 0;
tests.forEach(function (t) {
    try { t[1](); console.log("ok   " + t[0]); } catch (e) { failed++; console.log("FAIL " + t[0] + "\n     " + (e && e.stack || e)); }
});
if (failed) { process.exit(1); }
