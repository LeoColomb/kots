import ffi from "ffi";
import Struct from "ref-struct";
import { Stores } from "../schema/stores";
import { KotsApp } from "./";
import { Params } from "../server/params";
import { putObject } from "../util/s3";
import path from "path";
import tmp from "tmp";
import fs from "fs";
import { KotsAppRegistryDetails } from "../kots_app"
import { Cluster } from "../cluster";
import * as _ from "lodash";
import yaml from "js-yaml";
import { StatusServer } from "../airgap/status";
import { getDiffSummary } from "../util/utilities";
import { ReplicatedError } from "../server/errors";
import { createGitCommitForVersion } from "./gitops";

const GoString = Struct({
  p: "string",
  n: "longlong"
});

function kots() {
  return ffi.Library("/lib/kots.so", {
    EncryptString: [GoString, [GoString, GoString]],
    DecryptString: [GoString, [GoString, GoString]],
    RenderFile: ["void", [GoString, GoString, GoString, GoString]],
  });
}

export interface Update {
  cursor: string;
  versionLabel: string;
}

export async function kotsRenderFile(app: KotsApp, sequence: number, input: string, registryInfo: KotsAppRegistryDetails): Promise<string> {
  const filename = tmp.tmpNameSync();
  fs.writeFileSync(filename, input);

  // for the status server
  const tmpDir = tmp.dirSync();
  const archive = path.join(tmpDir.name, "archive.tar.gz");

  try {
    fs.writeFileSync(archive, await app.getArchive("" + sequence));

    const statusServer = new StatusServer();
    await statusServer.start(tmpDir.name);

    const socketParam = new GoString();
    socketParam["p"] = statusServer.socketFilename;
    socketParam["n"] = statusServer.socketFilename.length;

    const filepathParam = new GoString();
    filepathParam["p"] = filename;
    filepathParam["n"] = filename.length;

    const archivePathParam = new GoString();
    archivePathParam["p"] = archive;
    archivePathParam["n"] = archive.length;

    const registryJson = JSON.stringify(registryInfo)
    const registryJsonParam = new GoString();
    registryJsonParam["p"] = registryJson;
    registryJsonParam["n"] = registryJson.length;

    kots().RenderFile(socketParam, filepathParam, archivePathParam, registryJsonParam);
    await statusServer.connection();
    await statusServer.termination((resolve, reject, obj): boolean => {
      // Return true if completed
      if (obj.status === "terminated") {
        if (obj.exit_code !== -1) {
          resolve();
        } else {
          reject(new Error(obj.display_message));
        }
        return true;
      }
      return false;
    });

    const rendered = fs.readFileSync(filename);
    return rendered.toString();
  } finally {
    tmpDir.removeCallback();
  }
}

export async function kotsEncryptString(cipherString: string, message: string): Promise<string> {
  const cipherStringParam = new GoString();
  cipherStringParam["p"] = cipherString;
  cipherStringParam["n"] = Buffer.from(cipherString).length;

  const messageParam = new GoString();
  messageParam["p"] = message;
  messageParam["n"] = Buffer.from(message).length;

  const encrypted = kots().EncryptString(cipherStringParam, messageParam);

  if (encrypted["p"] === null) {
    throw new ReplicatedError("Failed to encrypt string via FFI call");
  }

  return encrypted["p"];
}

export async function kotsDecryptString(cipherString: string, message: string): Promise<string> {
  const cipherStringParam = new GoString();
  cipherStringParam["p"] = cipherString;
  cipherStringParam["n"] = Buffer.from(cipherString).length;

  const messageParam = new GoString();
  messageParam["p"] = message;
  messageParam["n"] = Buffer.from(message).length;

  const decrypted = kots().DecryptString(cipherStringParam, messageParam);

  if (decrypted["p"] === null) {
    throw new ReplicatedError("Failed to encrypt string via FFI call");
  }

  return decrypted["p"];
}

export function getK8sNamespace(): string {
  if (process.env["DEV_NAMESPACE"]) {
    return String(process.env["DEV_NAMESPACE"]);
  }
  if (process.env["POD_NAMESPACE"]) {
    return String(process.env["POD_NAMESPACE"]);
  }
  return "default";
}

export function getKotsadmNamespace(): string {
  if (process.env["POD_NAMESPACE"]) {
    return String(process.env["POD_NAMESPACE"]);
  }
  return "default";
}
