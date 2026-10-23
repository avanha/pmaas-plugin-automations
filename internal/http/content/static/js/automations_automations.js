/*
 * The buttons of the automations page: run one now (after asking), pause or resume it, and re-enable one that has been disabled. What each did is
 * said beside it if it did not work, and the page is loaded again when it did, since the page is stale then.
 *
 * It runs on old browsers, an iPad that no longer gets updates among them (Safari 9): plain ES5, with no arrow functions, classes, class
 * fields, let or const, async functions or fetch, none of which it has, and a script that uses one of them does not run at all, which is a
 * page of buttons that cannot be clicked. Nor does it have NodeList.forEach. Nothing here is slower or worse in a modern browser.
 *
 * The parts are given what they use from outside (the document, how a request is made, the question, how the page is reloaded), so that they
 * are tested with fakes: see js_test/automations_test.js.
 */
(function (global) {
    "use strict";

    // A NodeList from the old browsers has no forEach.
    function toArray(list) {
        return Array.prototype.slice.call(list);
    }

    // Posts the body as JSON, and calls done(error, status, text), error being set if there was no answer at all.
    function postJson(url, body, done) {
        var request = new global.XMLHttpRequest();

        request.open("POST", url, true);
        request.setRequestHeader("Content-Type", "application/json");

        request.onload = function () {
            done(null, request.status, request.responseText);
        };

        request.onerror = function () {
            done(new Error("the request could not be sent"));
        };

        request.send(JSON.stringify(body));
    }

    function Automations(doc, options) {
        options = options || {};
        this.doc = doc;
        this.post = options.post || postJson;
        this.confirm = options.confirm || function (message) {
            return global.confirm(message);
        };
        this.reload = options.reload || function () {
            global.location.reload();
        };
    }

    Automations.prototype.init = function () {
        var self = this;
        var root = this.doc.querySelector("div.entity-automations-automations");

        if (!root) {
            return false;
        }

        [["button.run-button", "run"], ["button.rearm-button", "rearm"], ["button.pause-button", "pause"]].forEach(function (pair) {
            toArray(root.querySelectorAll(pair[0])).forEach(function (button) {
                button.addEventListener("click", function () {
                    self[pair[1]](button);
                });
            });
        });

        return true;
    };

    // ask does what every button does: say nothing yet, disable the button, post, and then either say what went wrong and enable it again,
    // or hand what the server answered to onAnswer, which says whether the page is to be loaded again. failure is what is said, with the
    // status in brackets after it or a colon and the reason before there was an answer at all.
    Automations.prototype.ask = function (button, url, body, failure, onAnswer) {
        var self = this;
        var resultElement = button.parentNode.querySelector(".run-result");
        resultElement.textContent = "";
        button.disabled = true;

        this.post(url, body, function (error, status, text) {
            if (error) {
                console.error("Failed:", url, error);
                resultElement.textContent = failure + ": " + error;
                button.disabled = false;
                return;
            }

            if (status < 200 || status > 299) {
                // Say so on the page: a click that silently does nothing is worse than an error.
                console.error("Failed:", url, status, text);
                resultElement.textContent = failure + " (" + status + ")";
                button.disabled = false;
                return;
            }

            if (onAnswer && !onAnswer(text, resultElement)) {
                button.disabled = false;
                return;
            }

            self.reload();
        });
    };

    Automations.prototype.pause = function (button) {
        // The button says what it is now; the request is for the opposite.
        this.ask(button, "/plugins/automations/pause", {ID: button.dataset.id, Paused: button.dataset.paused !== "true"}, "Could not do that");
    };

    Automations.prototype.rearm = function (button) {
        this.ask(button, "/plugins/automations/rearm", {ID: button.dataset.id}, "Could not re-enable it", function (text, resultElement) {
            var result;

            try {
                result = JSON.parse(text);
            } catch (e) {
                resultElement.textContent = "Could not re-enable it: the answer was not understood";
                return false;
            }

            if (!result.Armed) {
                // It is a normal answer, not an error: say why nothing changed.
                resultElement.textContent = "Not re-enabled: " + result.Why;
                return false;
            }

            return true;
        });
    };

    Automations.prototype.run = function (button) {
        // It really does something: turns real things on and off. Ask first.
        if (!this.confirm("Run \"" + button.dataset.name + "\" now?")) {
            return;
        }

        // Whether it ran or not, the page is stale now, and shows which.
        this.ask(button, "/plugins/automations/run", {ID: button.dataset.id}, "Could not run it");
    };

    var exported = {Automations: Automations};

    if (typeof module !== "undefined" && module.exports) {
        module.exports = exported;
    }

    if (typeof document !== "undefined" && global.addEventListener) {
        new Automations(document).init();
    }
})(typeof window !== "undefined" ? window : globalThis);
