// JSON shapes returned by the hosuto Go backend (field names match its json tags:
// backend/internal/store for the persisted types, backend/internal/api for the response
// envelopes). The vocabularies mirror store.ValidLoader/ValidPolicy/ValidKind/ValidLevel —
// the browser must never invent a value the daemon would reject.

export type Loader = 'vanilla' | 'fabric' | 'neoforge' | 'paper';
export type JoinPolicy = 'whitelist' | 'open';
export type Level = 'play' | 'op';
export type GrantKind = 'adhoc' | 'contax' | 'holistic' | 'minecraft';
export type Env = 'required' | 'optional' | 'unsupported' | 'unknown';
// The full vocabulary of `systemctl is-active`, which runtime.State passes through verbatim.
// deactivating and reloading are easy to forget and neither is rare: stopping a modded server takes
// a while (hosuto-stop issues an RCON /stop and waits for the world to save), so "deactivating" is on
// screen for a good few seconds every single time one is shut down.
export type RunState = 'active' | 'inactive' | 'failed' | 'activating' | 'deactivating' | 'reloading';

export const LOADERS: Loader[] = ['vanilla', 'fabric', 'neoforge', 'paper'];

// Mirrors store.LoaderHasClientMods. Only fabric/neoforge produce something a PLAYER installs:
// Paper runs Bukkit plugins, which are server-side only, and vanilla runs nothing at all. The mod
// list and all three client exports hang off this one fact, so it is stated in exactly one place.
export function loaderHasClientMods(l: Loader | string): boolean {
  return l === 'fabric' || l === 'neoforge';
}

export interface Account {
  user: string; // Linux username — the join key to the whole landscape
  game: string;
  uuid: string; // dashed
  name: string; // the in-game name at link time
  verified: boolean; // ownership proven — always false for a name lookup
  linked: number;
}

export interface Grant {
  id: string;
  kind: GrantKind;
  ref: string; // contax: group id · holistic: OS group · minecraft: dashed uuid · adhoc: ""
  label: string;
  level: Level;
  members?: string[]; // adhoc: Linux usernames
  created: number;
}

export interface Mod {
  id: string;
  source: 'modrinth' | 'upload';
  projectId?: string;
  versionId?: string;
  name: string;
  filename: string;
  url?: string;
  sha1?: string;
  sha512?: string;
  size?: number;
  clientSide: Env;
  serverSide: Env;
  added: number;
}

export interface Server {
  id: string;
  slug: string;
  name: string;
  owner: string;
  game: string;
  mcVersion: string;
  loader: Loader;
  loaderVersion?: string;
  heapMB: number;
  port: number;
  rconPort: number;
  host: string; // <slug>.<zone> — the address a player types
  joinPolicy: JoinPolicy;
  grants?: Grant[];
  mods?: Mod[];
  created: number;
}

export interface Status {
  state: RunState;
  reachable: boolean; // a real Server List Ping succeeded
  online: number;
  max: number;
  sample?: string[];
  autostart: boolean; // comes up with the OS
  // The live server is running a mod set that no longer matches its record. The daemon only ever
  // reports this while the server is actually up (a stopped one reads mods/ fresh on its next start),
  // so the UI never has to reason about when it applies — it is true exactly when it matters.
  restartRequired?: boolean;
}

// A server as the API hands it over, with the caller's relationship to it attached.
export interface ServerView extends Server {
  owned: boolean;
  level?: Level; // the caller's level on a joinable server
  status?: Status;
}

export interface ServersResp {
  owned: ServerView[];
  joinable: ServerView[];
  account: Account | null;
  canHost: boolean;
}

// Why a server did or did not come up: the tail of its console log plus a short AI explanation.
// The log is always present; diagnosis is best-effort (empty when no AI credential is configured).
export interface Diagnose {
  state: RunState;
  log: string;
  diagnosis?: string;
  engine?: string;
  model?: string;
  diagnosisError?: 'no-credential' | 'failed';
}

export interface Info {
  service: string;
  version: string;
  user: string;
  zone: string; // the DNS zone every server's host hangs under
  canHost: boolean;
}

// A freshly minted pairing code: what the desktop app is handed so it can trade it for a token. host
// travels with it because the app needs to know WHERE to claim, and making the user recall their own
// server's address is a step that earns nothing.
export interface PairStart {
  code: string;
  expires: number; // unix seconds
  host: string;
}

// One entry of a server's player list. `user` is empty for a Minecraft account admitted directly:
// nobody in this landscape stands behind it yet, and `name`/`uuid` are all there is to show.
export interface Player {
  user: string;
  name?: string;
  uuid?: string;
  level: Level;
  hasAccount: boolean;
}

// A Minecraft account as Mojang spells it, resolved from a name the user typed.
export interface McProfile {
  uuid: string;
  name: string;
}

export interface MembersResp {
  policy: JoinPolicy;
  grants: Grant[];
  players: Player[];
}

// One currently-connected player, read from the server console. user is the holistic username when the
// name belongs to a linked account (so the UI can render that member's face); a guest has name only.
export interface OnlinePlayer {
  name: string;
  user?: string;
}
export interface OnlineResp {
  reachable: boolean;
  online: OnlinePlayer[];
}

export interface PolicyResp {
  joinPolicy: JoinPolicy;
  restartRequired: boolean;
}

export interface ModsResp {
  mods: Mod[];
  loader: Loader;
  hasClientMods: boolean;
}

export interface VersionResp {
  server: Server;
  dropped: string[]; // mods with no build for the new (version, loader) pair — reported, never hidden
}

export interface CatalogVersion {
  id: string;
  type: 'release' | 'snapshot' | 'old_beta' | 'old_alpha';
}
export interface CatalogVersionsResp {
  versions: CatalogVersion[];
}
export interface CatalogLoadersResp {
  versions: string[];
}

export interface ModHit {
  projectId: string;
  slug: string;
  title: string;
  description: string;
  iconUrl?: string;
  clientSide: Env;
  serverSide: Env;
  downloads: number;
}
export interface CatalogModsResp {
  mods: ModHit[];
}

export type Tab = 'reach' | 'players' | 'modding' | 'files' | 'ai' | 'export';

// ── creating a server ─────────────────────────────────────────────────────────────────

// The three ways a server comes into being. They are one act with three sources, not three
// features — hence one entry point in the UI and one shared set of rules in the daemon.
export type CreateMode = 'new' | 'template' | 'migrate';
// Where a migration reads from: an archive the user uploads, or a foreign host over FTP.
export type MigrateSource = 'upload' | 'ftp';

// A saved server recipe (store.Template). includeWorld says whether it is a starting point or a
// clone — the difference between a few megabytes and a few gigabytes, so the UI must show it.
export interface Template {
  id: string;
  name: string;
  owner: string;
  game: string;
  mcVersion: string;
  loader: Loader;
  loaderVersion?: string;
  heapMB: number;
  joinPolicy: JoinPolicy;
  mods?: Mod[];
  includeWorld: boolean;
  size: number;
  sourceSlug?: string;
  created: number;
}

export interface TemplatesResp {
  templates: Template[];
}

export type JobState = 'running' | 'done' | 'failed' | 'canceled';

// Background work that outlives the request that started it (jobs.Job). done/total count bytes or
// items depending on the phase; total 0 means "not yet known", which the UI renders as an
// indeterminate bar rather than a bar stuck at zero.
export interface Job {
  id: string;
  kind: 'import' | 'template';
  owner: string;
  state: JobState;
  phase: string;
  message?: string;
  done: number;
  total: number;
  serverId?: string;
  notes?: string[];
  error?: string;
  started: number;
  ended?: number;
}

export interface JobsResp {
  jobs: Job[];
}

// One turn in a server's shared "Ask AI" thread. Persisted by hosuto, visible to every operator of
// the server; a user turn carries the author so the shared log shows who asked.
export interface ChatMsg {
  role: 'user' | 'assistant';
  content: string;
  author?: string; // user turn: the operator's holistic (Linux) username
  name?: string; // user turn: the operator's Minecraft name at post time (UI falls back to author)
  engine?: string; // assistant turn: which aigentic engine answered
  model?: string; // assistant turn: which concrete model answered
  ts: number;
}
// One shared conversation for a server, with its full message list.
export interface Conversation {
  id: string;
  title: string;
  updated: number;
  messages: ChatMsg[];
}
// The lightweight sidebar view of a conversation (no message bodies).
export interface ConvSummary {
  id: string;
  title: string;
  updated: number;
  count: number;
}
export interface ChatsResp {
  conversations: ConvSummary[];
}
// Live presence of an operator in a conversation (from the SSE stream).
export interface PresenceEntry {
  author: string;
  name?: string;
  state: 'typing' | 'working';
}
