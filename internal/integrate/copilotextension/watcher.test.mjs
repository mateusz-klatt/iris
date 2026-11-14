import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import test from "node:test";

import { attentionPrompt, interactiveHost, isAttentionLine, notInteractive, runExtension, startWatcher } from "./watcher.mjs";

function harness(t, { send, log } = {}) {
    const calls = [];
    const listeners = new Map();
    const child = new EventEmitter();
    child.stdout = new PassThrough();
    child.stderr = new PassThrough();
    child.kill = () => {
        calls.push(["kill"]);
        return true;
    };
    const session = {
        on(event, handler) {
            listeners.set(event, handler);
        },
        send:
            send ??
            (async (message) => {
                calls.push(["send", message]);
                return "message-id";
            }),
        log: log ?? (async (...args) => calls.push(["log", ...args])),
    };
    const stop = startWatcher({
        iris: "/tools/iris with spaces",
        session,
        sessionId: "copilot-session",
        environment: { IRIS_HUB_URL: "https://hub.example" },
        spawn: (...args) => {
            calls.push(["spawn", ...args]);
            return child;
        },
    });
    t.after(() => stop());
    return { calls, child, listeners, stop };
}

test("only positive Iris attention lines wake the session", () => {
    for (const line of [
        "Iris: 1 new message. Use the inbox tool.",
        "Iris: 12 new messages. Use the inbox tool.",
        "Iris: a path you were refused is free now. Use the claims tool.",
        "Iris: 2 paths you were refused are free now. Use the claims tool.",
    ]) {
        assert.equal(isAttentionLine(line), true, line);
    }
    for (const line of [
        "Iris: 0 new messages. Use the inbox tool.",
        "Iris: notifications stopped: the hub is unavailable.",
        "Iris: notifications resumed.",
        "Iris: claim released.",
    ]) {
        assert.equal(isAttentionLine(line), false, line);
    }
});

test("starts a session-bound watch and sends only a fixed automation prompt", async (t) => {
    const { calls, child } = harness(t);
    const [, executable, args, options] = calls[0];
    assert.equal(executable, "/tools/iris with spaces");
    assert.deepEqual(args, ["watch", "--client", "copilot"]);
    assert.equal(options.env.COPILOT_AGENT_SESSION_ID, "copilot-session");
    assert.equal(options.env.IRIS_HUB_URL, "https://hub.example");

    child.stdout.write("Iris: 1 new message. Use the inbox tool. secret body\n");
    await new Promise((resolve) => setImmediate(resolve));
    const send = calls.find((call) => call[0] === "send");
    assert.deepEqual(send, ["send", { prompt: attentionPrompt, source: "automation-iris", mode: "enqueue" }]);
    assert.equal(send[1].prompt.includes("secret body"), false);
    assert.equal(attentionPrompt.includes("message body"), true);
});

test("wakes once when idle and defers attention received during a turn", async (t) => {
    const calls = [];
    const { child, listeners } = harness(t, {
        send: async (message) => {
            calls.push(["send", message]);
            return "message-id";
        },
    });
    listeners.get("assistant.turn_start")();
    child.stdout.write(
        "Iris: 1 new message. Use the inbox tool.\nIris: a path you were refused is free now. Use the claims tool.\n"
    );
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(calls.length, 0);

    listeners.get("session.idle")();
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(calls.length, 1);
    assert.deepEqual(calls[0], [
        "send",
        { prompt: attentionPrompt, source: "automation-iris", mode: "enqueue" },
    ]);

    listeners.get("assistant.turn_start")();
    child.stdout.write("Iris: 2 new messages. Use the inbox tool.\n");
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(calls.length, 1);
    listeners.get("session.idle")();
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(calls.length, 2);
});

test("logs the monitor's status lines without waking the model", async (t) => {
    const { calls, child } = harness(t);
    child.stdout.write("Iris: notifications stopped: the hub is unavailable.\n");
    child.stdout.write("Iris: notifications still stopped: run iris doctor.\n");
    child.stdout.write("Iris: notifications resumed.\n");
    child.stdout.write("a line that is not from Iris\n");
    await new Promise((resolve) => setImmediate(resolve));
    assert.deepEqual(
        calls.filter((call) => call[0] === "log").map((call) => [call[1], call[2].level]),
        [
            ["Iris: notifications stopped: the hub is unavailable.", "warning"],
            ["Iris: notifications still stopped: run iris doctor.", "warning"],
            ["Iris: notifications resumed.", "info"],
        ]
    );
    assert.equal(calls.some((call) => call[0] === "send"), false);
});

// iris watch prints this once and keeps running, so no exit is reported for it.
test("logs a token the hub rejected as a warning without waking the model", async (t) => {
    const { calls, child } = harness(t);
    const rejected =
        "Iris: the hub rejected this host's token, so notifications have stopped. Tell the user to run `iris doctor`.";
    child.stdout.write(`${rejected}\n`);
    await new Promise((resolve) => setImmediate(resolve));
    assert.deepEqual(
        calls.filter((call) => call[0] === "log").map((call) => [call[1], call[2].level]),
        [[rejected, "warning"]]
    );
    assert.equal(calls.some((call) => call[0] === "send"), false);
});

test("reports an unconfirmed wake without retrying it", async (t) => {
    let sends = 0;
    const { calls, child } = harness(t, {
        send: async () => {
            sends++;
            throw new Error("connection lost");
        },
    });
    child.stdout.write("Iris: 1 new message. Use the inbox tool.\n");
    await new Promise((resolve) => setImmediate(resolve));
    await new Promise((resolve) => setImmediate(resolve));
    const failure = calls.find((call) => call[0] === "log");
    assert.equal(failure[1].includes("could not confirm"), true);
    assert.equal(failure[1].includes("connection lost"), true);
    assert.equal(failure[2].level, "error");
    assert.equal(sends, 1);
});

test("reports an unconfirmed wake whose failure carries no message", async (t) => {
    const { calls, child } = harness(t, {
        send: async () => {
            throw { code: 7 };
        },
    });
    child.stdout.write("Iris: 1 new message. Use the inbox tool.\n");
    await new Promise((resolve) => setImmediate(resolve));
    await new Promise((resolve) => setImmediate(resolve));
    const failure = calls.find((call) => call[0] === "log");
    assert.equal(failure[1], "Iris could not confirm that Copilot accepted the wake; check the session and inbox.");
    assert.equal(failure[2].level, "error");
});

test("falls back to stderr when the session cannot take a log entry", async (t) => {
    const errors = [];
    t.mock.method(console, "error", (...args) => errors.push(args));
    const { child } = harness(t, {
        log: async () => {
            throw new Error("session closed");
        },
    });
    child.stdout.write("Iris: notifications stopped: the hub is unavailable.\n");
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(errors.length, 1);
    assert.match(errors[0][0], /could not report monitor status/);
    assert.equal(errors[0][1].message, "session closed");
});

test("reports monitor startup and unexpected exit once", async (t) => {
    const first = harness(t);
    first.child.emit("error", new Error("spawn failed"));
    first.child.emit("exit", 1, null);
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(first.calls.filter((call) => call[0] === "log").length, 1);

    const second = harness(t);
    second.child.emit("exit", 1, null);
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(second.calls.filter((call) => call[0] === "log").length, 1);
});

test("does not report a normal stop as a monitor failure", async (t) => {
    const { calls, child, stop } = harness(t);
    stop();
    child.emit("exit", 0, null);
    await new Promise((resolve) => setImmediate(resolve));
    assert.deepEqual(calls.filter((call) => call[0] === "log"), []);
    assert.equal(calls.filter((call) => call[0] === "kill").length, 1);
});

test("requires a joined session and executable before starting", () => {
    assert.throws(() => startWatcher({}), /requires an executable and a joined session/);
});

test("binds the watcher to the joined session and stops it once on SIGINT or SIGTERM", async () => {
    const calls = [];
    const signals = new EventEmitter();
    const child = new EventEmitter();
    child.stdout = new PassThrough();
    child.stderr = new PassThrough();
    child.kill = () => calls.push(["kill"]);
    const stop = await runExtension({
        joinSession: async () => ({
            sessionId: "joined-session",
            capabilities: { ui: { elicitation: true, canvases: true } },
            on() {},
            send: async () => {},
            log: async () => {},
        }),
        iris: "/tools/iris",
        signals,
        environment: {},
        spawn: (...args) => {
            calls.push(["spawn", ...args]);
            return child;
        },
    });
    assert.equal(calls[0][3].env.COPILOT_AGENT_SESSION_ID, "joined-session");
    assert.equal(signals.listenerCount("SIGINT"), 1);
    assert.equal(signals.listenerCount("SIGTERM"), 1);

    signals.emit("SIGTERM");
    signals.emit("SIGINT");
    stop();
    assert.equal(calls.filter((call) => call[0] === "kill").length, 1);
});

// Copilot CLI 1.0.112 reports canvases true in its TUI and false under -p (ADR 031). Only exactly true
// starts the watcher; anything else, a getter that throws among it, logs one line and sends nothing.
test("starts the watcher only in an interactive host", async () => {
    const throwing = {};
    Object.defineProperty(throwing, "ui", {
        get() {
            throw new Error("capabilities unavailable");
        },
    });
    for (const [name, capabilities] of [
        ["print mode", { ui: { elicitation: true, canvases: false } }],
        ["missing", undefined],
        ["no ui", {}],
        ["not a boolean", { ui: { canvases: "true" } }],
        ["throwing", throwing],
    ]) {
        const logs = [];
        const signals = new EventEmitter();
        const stop = await runExtension({
            joinSession: async () => ({
                sessionId: "print-session",
                capabilities,
                on: () => assert.fail("no events are watched"),
                send: () => assert.fail("nothing is sent"),
                log: async (...args) => logs.push(args),
            }),
            iris: "/tools/iris",
            signals,
            spawn: () => assert.fail(`${name}: a watcher must not start`),
        });
        stop();
        stop();
        assert.deepEqual(logs, [[notInteractive, { level: "info" }]], name);
        assert.equal(signals.listenerCount("SIGTERM"), 0, name);
        assert.equal(signals.listenerCount("SIGINT"), 0, name);
        assert.equal(interactiveHost({ capabilities }), false, name);
    }
    assert.equal(interactiveHost({ capabilities: { ui: { canvases: true } } }), true);
});

test("reports a failed log line in a host that is not interactive", async () => {
    const errors = [];
    const original = console.error;
    console.error = (...args) => errors.push(args);
    try {
        await runExtension({
            joinSession: async () => ({ sessionId: "print-session", log: async () => Promise.reject(new Error("closed")) }),
            iris: "/tools/iris",
            signals: new EventEmitter(),
            spawn: () => assert.fail("a watcher must not start"),
        });
        await new Promise((resolve) => setImmediate(resolve));
    } finally {
        console.error = original;
    }
    assert.equal(errors.length, 1);
    assert.match(errors[0][0], /could not report monitor status/);
});

test("starts no watcher when the session cannot be joined", async () => {
    const signals = new EventEmitter();
    await assert.rejects(
        runExtension({
            joinSession: async () => {
                throw new Error("no foreground session");
            },
            iris: "/tools/iris",
            signals,
            spawn: () => assert.fail("a watcher must not start without a session"),
        }),
        /no foreground session/
    );
    assert.equal(signals.listenerCount("SIGTERM"), 0);
});
