// JSON shapes returned by the hosuto Go backend (field names match its json tags:
// backend/internal/store for the persisted types, backend/internal/api for the response
// envelopes). The vocabularies mirror store.ValidLoader/ValidPolicy/ValidKind/ValidLevel —
// the browser must never invent a value the daemon would reject.

export type Loader = 'vanilla' | 'fabric' | 'neoforge' | 'paper';
export type JoinPolicy = 'whitelist' | 'open';
export type Level = 'play' | 'op';
export type GrantKind = 'adhoc' | 'contax' | 'holistic';
export type Env = 'required' | 'optional' | 'unsupported' | 'unknown';
export type RunState = 'active' | 'inactive' | 'failed' | 'activating';

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
  ref: string; // contax: group id · holistic: OS group · adhoc: ""
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

export interface Info {
  service: string;
  version: string;
  user: string;
  zone: string; // the DNS zone every server's host hangs under
  canHost: boolean;
}

export interface Player {
  user: string;
  name?: string;
  uuid?: string;
  level: Level;
  hasAccount: boolean;
}

export interface MembersResp {
  policy: JoinPolicy;
  grants: Grant[];
  players: Player[];
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

// One turn in a server's shared "Ask AI" thread. Persisted by hosuto, visible to every operator of
// the server; a user turn carries the author so the shared log shows who asked.
export interface ChatMsg {
  role: 'user' | 'assistant';
  content: string;
  author?: string;
  ts: number;
}
export interface ChatResp {
  messages: ChatMsg[];
}
