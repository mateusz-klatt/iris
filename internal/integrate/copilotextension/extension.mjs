import { joinSession } from "@github/copilot-sdk/extension";
import { runExtension } from "./watcher.mjs";

await runExtension({ joinSession, iris: "__IRIS_EXECUTABLE__" });
