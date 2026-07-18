import type { ServiceApiClient } from '@holistic/ui';

// The URL of a player's Minecraft face, rendered by hosuto from their skin.
//
// hosuto owns the Linux user → Minecraft account mapping, so it is the only service that can render
// this — and it does so itself rather than pointing the browser at crafatar or mc-heads: those are an
// availability dependency (crafatar's public instance is currently down) and they would leak every
// member's Mojang UUID to a third party.
//
// `ref` is a MEMBER's Linux username wherever there is one, so a member's UUID never appears in a URL
// the browser keeps. A directly-admitted Minecraft account has no member behind it and no username to
// name it by, so there `ref` is the dashed UUID — which discloses nothing, being the identity the
// owner typed in to admit them.
//
// Ask for twice the rendered size: the face is 8×8 pixel art scaled with nearest-neighbour, so a
// 2× source keeps it crisp on a HiDPI display instead of letting the browser resample it to mush.
//
// A member with no linked account (or no skin) gets a 404, and <Avatar> falls back to their initials
// on the image error — which is the honest outcome, not a stand-in Steve they never chose.
export function faceUrl(api: ServiceApiClient, ref: string, size: number): string {
  return api.url(`avatar/${encodeURIComponent(ref)}?size=${size}`);
}
