import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import test from "node:test";

import { attentionPrompt, isAttentionLine, runExtension, startWatcher } from "./watcher.mjs";

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

test("logs stopped monitors without waking the model", async (t) => {
    const { calls, child } = harness(t);
    child.stdout.write("Iris: notifications stopped: the hub is unavailable.\n");
    child.stdout.write("Iris: notifications still stopped: run iris doctor.\n");
    child.stdout.write("Iris: notifications resumed.\n");
    await new Promise((resolve) => setImmediate(resolve));
    assert.deepEqual(
        calls.filter((call) => call[0] === "log").map((call) => call[1]),
        [
            "Iris: notifications stopped: the hub is unavailable.",
            "Iris: notifications still stopped: run iris doctor.",
        ]
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
        joinSession: async () => ({ sessionId: "joined-session", on() {}, send: async () => {}, log: async () => {} }),
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
