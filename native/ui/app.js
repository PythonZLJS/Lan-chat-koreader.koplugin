/* ES5 only; preferences contain no chat content or credentials. */
(function () {
    function init() {
        var root = document.documentElement, body = document.body;
        var buttons = document.getElementsByTagName("button"), i;
        var size = "normal", spacing = "comfortable", panel = document.getElementById("display-options");
        var pageClass = body.className;
        function save() {
            try { window.localStorage.setItem("nc-display", size + ":" + spacing); } catch (ignored) {}
        }
        function apply() {
            body.style.fontSize = size === "small" ? "85%" : (size === "large" ? "120%" : "100%");
            body.className = pageClass + (spacing === "compact" ? " compact" : "");
            for (var n = 0; n < buttons.length; n += 1) {
                var value = buttons[n].getAttribute("data-size") || buttons[n].getAttribute("data-spacing");
                if (value) {
                    var active = value === size || value === spacing;
                    buttons[n].className = "button" + (active ? " active" : "");
                    buttons[n].setAttribute("aria-pressed", active ? "true" : "false");
                }
            }
        }
        try {
            var saved = (window.localStorage.getItem("nc-display") || "").split(":");
            if (/^(small|normal|large)$/.test(saved[0])) { size = saved[0]; }
            if (/^(compact|comfortable)$/.test(saved[1])) { spacing = saved[1]; }
        } catch (ignored) {}
        for (i = 0; i < buttons.length; i += 1) {
            if (buttons[i].getAttribute("data-toggle")) {
                buttons[i].setAttribute("aria-pressed", "false");
                buttons[i].onclick = function () {
                    var input = document.getElementById(this.getAttribute("data-toggle"));
                    if (!input) { return; }
                    var show = input.type === "password";
                    try { input.type = show ? "text" : "password"; } catch (ignored) { return; }
                    this.innerHTML = show ? "Hide" : "Show";
                    this.setAttribute("aria-pressed", show ? "true" : "false");
                };
            } else if (buttons[i].id === "display-toggle") {
                buttons[i].onclick = function () {
                    var open = panel.className.indexOf("is-open") < 0;
                    panel.className = "display-options" + (open ? " is-open" : "");
                    this.setAttribute("aria-expanded", open ? "true" : "false");
                };
            } else if (buttons[i].getAttribute("data-size") || buttons[i].getAttribute("data-spacing")) {
                buttons[i].onclick = function () {
                    size = this.getAttribute("data-size") || size;
                    spacing = this.getAttribute("data-spacing") || spacing;
                    apply(); save();
                };
            }
        }
        apply();
        var compose = document.getElementById("compose"), composeToggle = document.getElementById("compose-toggle");
        var composeClose = document.getElementById("compose-close");
        if (compose && composeToggle && composeClose) {
            var setCompose = function (open) {
                compose.className = "compose" + (open ? " is-open" : "");
                composeToggle.setAttribute("aria-expanded", open ? "true" : "false");
            };
            var links = document.getElementsByTagName("a");
            for (i = 0; i < links.length; i += 1) {
                if (links[i].getAttribute("data-compose") === "open") {
                    links[i].onclick = function () { setCompose(true); };
                }
            }
            composeClose.onclick = function () {
                setCompose(false);
                // Closing keeps the unsent text in memory on this page.
                composeToggle.focus();
                window.location.hash = "conversation";
            };
            setCompose(window.location.hash === "#compose");
            window.addEventListener("hashchange", function () {
                if (window.location.hash === "#compose") { setCompose(true); }
            }, false);
        }
        // Enhance only after all controls are wired; forms remain visible if
        // an older engine fails during setup. Never summon the keyboard on load.
        root.className = "js";
        var recipient = document.getElementById("recipient"), note = document.getElementById("recipient-note");
        if (recipient && note) {
            recipient.onchange = function () {
                var text = recipient.value ? "Private message to " + recipient.options[recipient.selectedIndex].text + "." : "Everyone in this chat can read this message.";
                // Names are untrusted: create a text node, never HTML.
                while (note.firstChild) { note.removeChild(note.firstChild); }
                note.appendChild(document.createTextNode(text));
            };
        }
        // E-ink can leave visible password text behind; hide it on page exit too.
        window.addEventListener("pagehide", function () {
            for (var n = 0; n < buttons.length; n += 1) {
                var id = buttons[n].getAttribute("data-toggle");
                if (id) {
                    var input = document.getElementById(id);
                    if (input) { try { input.type = "password"; } catch (ignored) {} }
                    buttons[n].innerHTML = "Show";
                    buttons[n].setAttribute("aria-pressed", "false");
                }
            }
        }, false);
    }
    if (document.readyState === "loading") { document.addEventListener("DOMContentLoaded", init, false); }
    else { init(); }
}());
