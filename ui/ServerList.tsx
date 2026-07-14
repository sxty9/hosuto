// ServerList is hosuto's root view: the servers you own, and the servers other people added you
// to. The second list is the whole point of the contax mapping — hosuto owns the Linux-user →
// Minecraft-account link, so "Bob added you" can become a whitelist entry without anyone typing a
// UUID. It is also where the account itself gets linked, because without it a member cannot be
// whitelisted anywhere and every other surface here is useless to them.
import { useCallback, useEffect, useState } from 'react';
import {
  Avatar,
  Badge,
  Box,
  Button,
  ContextMenu,
  EmptyState,
  Field,
  Autocomplete,
  Input,
  Modal,
  Panel,
  PlusIcon,
  SegmentedControl,
  ServerIcon,
  Spinner,
  Stack,
  Text,
  TrashIcon,
  cn,
  useLiveQuery,
  useT,
  userHasRight,
  type AutocompleteOption,
  type MenuItem,
  type SegmentedOption,
  type ServiceContextProps,
  type TranslateFn,
} from '@holistic/ui';
import {
  LOADERS,
  type Account,
  type CatalogLoadersResp,
  type CatalogVersionsResp,
  type Info,
  type Loader,
  type RunState,
  type ServerView,
  type ServersResp,
} from './types';
import { faceUrl } from './face';

const HOST = 'hp_hosuto_host';
const PLAY = 'hp_hosuto_play';
const ADMIN = 'hp_hosuto_admin';

// Mirrors store.SlugRe. The slug is a public DNS label, so the rule is the daemon's, not ours —
// checking it here only saves a round trip, it never decides anything.
const SLUG_RE = /^[a-z][a-z0-9-]{1,31}$/;

const DOT: Record<RunState, string> = {
  active: 'bg-success',
  activating: 'bg-warning',
  inactive: 'bg-fill/40',
  failed: 'bg-danger',
};

type Action = 'start' | 'stop' | 'restart';

interface ListProps extends ServiceContextProps {
  onOpen: (id: string) => void;
}

export function ServerList({ onOpen, ...props }: ListProps) {
  const { user, api } = props;
  const t = useT();
  const q = useLiveQuery<ServersResp>(() => api.get<ServersResp>('servers'), 10000, []);
  const [creating, setCreating] = useState(false);

  const canHost = userHasRight(user, HOST);
  const canPlay = userHasRight(user, PLAY);
  const canAdmin = userHasRight(user, ADMIN);

  const owned = q.data?.owned ?? [];
  const joinable = q.data?.joinable ?? [];

  if (q.loading && !q.data) {
    return (
      <Stack direction="row" align="center" gap={2}>
        <Spinner />
        <Text color="secondary">{t('hosuto.loading')}</Text>
      </Stack>
    );
  }

  return (
    <Stack gap={5}>
      <AccountPanel {...props} account={q.data?.account ?? null} canPlay={canPlay} onChanged={q.refresh} />

      {canHost && (
        <Stack gap={3}>
          <Stack direction="row" align="center" justify="between">
            <Text variant="title3" weight="semibold">
              {t('hosuto.myServers')}
            </Text>
            <Button variant="primary" size="sm" iconLeft={<PlusIcon />} onClick={() => setCreating(true)}>
              {t('hosuto.newServer')}
            </Button>
          </Stack>
          {owned.length === 0 ? (
            <EmptyState icon={<ServerIcon />} title={t('hosuto.noOwned')} />
          ) : (
            <Stack gap={2}>
              {owned.map((srv) => (
                <ServerRow
                  key={srv.id}
                  {...props}
                  srv={srv}
                  t={t}
                  canControl
                  canDelete={canHost}
                  onOpen={onOpen}
                  onChanged={q.refresh}
                />
              ))}
            </Stack>
          )}
        </Stack>
      )}

      {canPlay && (
        <Stack gap={3}>
          <Text variant="title3" weight="semibold">
            {t('hosuto.joinableServers')}
          </Text>
          {joinable.length === 0 ? (
            <EmptyState icon={<ServerIcon />} title={t('hosuto.noJoinable')} />
          ) : (
            <Stack gap={2}>
              {joinable.map((srv) => (
                <ServerRow
                  key={srv.id}
                  {...props}
                  srv={srv}
                  t={t}
                  // An op-level member may drive the lifecycle of a server they were added to;
                  // an admin may drive anything. Deleting stays with the owner (and admins).
                  canControl={srv.level === 'op' || canAdmin}
                  canDelete={canAdmin && canHost}
                  onOpen={onOpen}
                  onChanged={q.refresh}
                />
              ))}
            </Stack>
          )}
        </Stack>
      )}

      {creating && (
        <CreateServerModal
          {...props}
          onClose={() => setCreating(false)}
          onCreated={(id) => {
            q.refresh();
            onOpen(id);
          }}
        />
      )}
    </Stack>
  );
}

// ── account ───────────────────────────────────────────────────────────────────────────

function AccountPanel({
  api,
  ui,
  account,
  canPlay,
  onChanged,
}: Pick<ServiceContextProps, 'api' | 'ui'> & {
  account: Account | null;
  canPlay: boolean;
  onChanged: () => void;
}) {
  const t = useT();
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);

  if (!canPlay) return null;

  async function link() {
    if (!name.trim()) return;
    setBusy(true);
    try {
      await api.put<Account>('account', { name: name.trim() });
      ui.toast({ title: t('hosuto.linked'), variant: 'success' });
      setName('');
      onChanged();
    } catch (e) {
      ui.toast({ title: t('hosuto.linkFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  async function unlink() {
    const ok = await ui.confirm({ title: t('hosuto.unlinkTitle'), danger: true, confirmLabel: t('hosuto.unlink') });
    if (!ok) return;
    try {
      await api.del('account');
      ui.toast({ title: t('hosuto.unlinked'), variant: 'success' });
      onChanged();
    } catch (e) {
      ui.toast({ title: t('hosuto.actionFailed'), description: (e as Error).message, variant: 'error' });
    }
  }

  // Linked: stay out of the way — a name, a face, and a way back out.
  if (account) {
    return (
      <Stack direction="row" align="center" gap={2}>
        <Avatar name={account.name} src={faceUrl(api, account.user, 48)} size={24} />
        <Text variant="footnote" color="secondary">
          {account.name}
        </Text>
        <Button variant="ghost" size="sm" onClick={unlink}>
          {t('hosuto.unlink')}
        </Button>
      </Stack>
    );
  }

  return (
    <Panel title={t('hosuto.accountTitle')} className="p-4">
      <Stack direction="row" align="center" gap={2}>
        <Input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder={t('hosuto.ingameName')}
          onKeyDown={(e) => e.key === 'Enter' && void link()}
        />
        <Button variant="primary" loading={busy} onClick={link}>
          {t('hosuto.link')}
        </Button>
      </Stack>
    </Panel>
  );
}

// ── one row ───────────────────────────────────────────────────────────────────────────

function ServerRow({
  api,
  ui,
  srv,
  t,
  canControl,
  canDelete,
  onOpen,
  onChanged,
}: Pick<ServiceContextProps, 'api' | 'ui'> & {
  srv: ServerView;
  t: TranslateFn;
  canControl: boolean;
  canDelete: boolean;
  onOpen: (id: string) => void;
  onChanged: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const state: RunState = srv.status?.state ?? 'inactive';
  const online = srv.status?.online ?? 0;
  const max = srv.status?.max ?? 0;

  async function act(action: Action) {
    setBusy(true);
    try {
      await api.post(`servers/${srv.id}/${action}`);
      ui.toast({ title: t(`hosuto.did.${action}`), variant: 'success' });
      onChanged();
    } catch (e) {
      ui.toast({ title: t('hosuto.actionFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  async function remove() {
    const ok = await ui.confirm({
      title: t('hosuto.deleteTitle', { name: srv.name }),
      description: t('hosuto.deleteBody'),
      danger: true,
      confirmLabel: t('hosuto.delete'),
    });
    if (!ok) return;
    setBusy(true);
    try {
      await api.del(`servers/${srv.id}`);
      ui.toast({ title: t('hosuto.deleted'), variant: 'success' });
      onChanged();
    } catch (e) {
      ui.toast({ title: t('hosuto.actionFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  const running = state === 'active' || state === 'activating';

  const items: MenuItem[] = [{ id: 'open', label: t('hosuto.open'), onSelect: () => onOpen(srv.id) }];
  if (canControl) {
    items.push({ id: 'start', label: t('hosuto.start'), disabled: busy || running, separatorBefore: true, onSelect: () => void act('start') });
    items.push({ id: 'stop', label: t('hosuto.stop'), disabled: busy || !running, onSelect: () => void act('stop') });
    items.push({ id: 'restart', label: t('hosuto.restart'), disabled: busy || !running, onSelect: () => void act('restart') });
  }
  if (canDelete) {
    items.push({
      id: 'delete',
      label: t('hosuto.delete'),
      icon: <TrashIcon />,
      danger: true,
      disabled: busy,
      separatorBefore: true,
      onSelect: () => void remove(),
    });
  }

  return (
    <ContextMenu items={items}>
      <Box>
        <Panel interactive className="cursor-pointer p-3" onClick={() => onOpen(srv.id)}>
          <Stack direction="row" align="center" justify="between" gap={3} wrap>
            <Stack direction="row" align="center" gap={3}>
              <Box className={cn('h-2.5 w-2.5 shrink-0 rounded-full', DOT[state])} />
              <Stack gap={0}>
                <Text weight="semibold">{srv.name}</Text>
                <Text variant="caption" color="tertiary">
                  {srv.host}
                </Text>
              </Stack>
              {state === 'active' && (
                <Badge variant={online > 0 ? 'success' : 'neutral'}>
                  {online}/{max}
                </Badge>
              )}
            </Stack>

            {canControl && (
              // stopPropagation: these buttons act on the row, they do not open it.
              <Stack direction="row" align="center" gap={1} onClick={(e) => e.stopPropagation()}>
                <Button variant="ghost" size="sm" disabled={busy || running} onClick={() => void act('start')}>
                  {t('hosuto.start')}
                </Button>
                <Button variant="ghost" size="sm" disabled={busy || !running} onClick={() => void act('stop')}>
                  {t('hosuto.stop')}
                </Button>
                <Button variant="ghost" size="sm" disabled={busy || !running} onClick={() => void act('restart')}>
                  {t('hosuto.restart')}
                </Button>
              </Stack>
            )}
          </Stack>
        </Panel>
      </Box>
    </ContextMenu>
  );
}

// ── create ────────────────────────────────────────────────────────────────────────────

function CreateServerModal({
  api,
  ui,
  onClose,
  onCreated,
}: Pick<ServiceContextProps, 'api' | 'ui'> & { onClose: () => void; onCreated: (id: string) => void }) {
  const t = useT();
  const [name, setName] = useState('');
  const [slug, setSlug] = useState('');
  const [mcVersion, setMcVersion] = useState('');
  const [loader, setLoader] = useState<Loader>('fabric');
  const [loaderVersion, setLoaderVersion] = useState('');
  const [heap, setHeap] = useState('2048');
  const [busy, setBusy] = useState(false);

  const [zone, setZone] = useState('');
  const [releases, setReleases] = useState<string[]>([]);
  const [loaderVersions, setLoaderVersions] = useState<string[]>([]);
  const [loadersLoading, setLoadersLoading] = useState(false);

  // The catalogue is the daemon's, not ours: versions come from Mojang's manifest and loader
  // builds from each loader's own index, both proxied and cached by the backend.
  useEffect(() => {
    api.get<Info>('info').then((i) => setZone(i.zone)).catch(() => setZone(''));
    api
      .get<CatalogVersionsResp>('catalog/versions')
      .then((r) => setReleases((r.versions ?? []).filter((v) => v.type === 'release').map((v) => v.id)))
      .catch(() => setReleases([]));
  }, [api]);

  const latest = releases[0] ?? '';
  const mc = mcVersion.trim() || latest;

  // Pre-fill the newest release once the catalogue loads, so the field shows the value that will
  // actually be used instead of a gray hint the user has to trust is adopted. Only an untouched
  // field is filled — once they type, their choice stands.
  useEffect(() => {
    if (!mcVersion && releases.length > 0) setMcVersion(releases[0]);
  }, [releases, mcVersion]);

  useEffect(() => {
    if (loader === 'vanilla' || !mc) {
      setLoaderVersions([]);
      setLoadersLoading(false);
      return;
    }
    let live = true;
    setLoadersLoading(true);
    api
      .get<CatalogLoadersResp>(`catalog/loaders?loader=${encodeURIComponent(loader)}&mcVersion=${encodeURIComponent(mc)}`)
      .then((r) => live && setLoaderVersions(r.versions ?? []))
      .catch(() => live && setLoaderVersions([]))
      .finally(() => live && setLoadersLoading(false));
    return () => {
      live = false;
    };
  }, [api, loader, mc]);

  // Same pre-fill for the loader build. The list reloads on every loader/version change (and the
  // loader switch clears the field, below), so this re-fills the newest build for each combination —
  // and the value shown is the value sent.
  useEffect(() => {
    if (loader !== 'vanilla' && !loaderVersion && loaderVersions.length > 0) {
      setLoaderVersion(loaderVersions[0]);
    }
  }, [loaderVersions, loader, loaderVersion]);

  // Mod loaders lag weeks behind a Minecraft release, so "newest Minecraft + NeoForge" — the most
  // natural thing to pick, and what this form defaults to — often has no builds at all. Say so at the
  // field instead of letting the user press Create and collect a failure. The daemon refuses the same
  // combination anyway; this only saves them the round trip.
  const unsupported = loader !== 'vanilla' && !!mc && !loadersLoading && loaderVersions.length === 0;

  const searchVersions = useCallback(
    async (q: string): Promise<AutocompleteOption[]> =>
      releases.filter((v) => v.includes(q)).slice(0, 40).map((v) => ({ id: v, label: v })),
    [releases],
  );
  const searchLoaderVersions = useCallback(
    async (q: string): Promise<AutocompleteOption[]> =>
      loaderVersions.filter((v) => v.includes(q)).slice(0, 40).map((v) => ({ id: v, label: v })),
    [loaderVersions],
  );

  const slugBad = slug.length > 0 && !SLUG_RE.test(slug);

  async function create() {
    if (!slug || slugBad) {
      ui.toast({ title: t('hosuto.slugRule'), variant: 'error' });
      return;
    }
    if (!mc) {
      ui.toast({ title: t('hosuto.pickVersion'), variant: 'error' });
      return;
    }
    setBusy(true);
    try {
      const srv = await api.post<ServerView>('servers', {
        name: name.trim() || slug,
        slug,
        mcVersion: mc,
        loader,
        loaderVersion: loaderVersion.trim(), // empty: the daemon takes the newest build
        heapMB: Number(heap) || 0,
      });
      ui.toast({ title: t('hosuto.created'), variant: 'success' });
      onCreated(srv.id);
      onClose();
    } catch (e) {
      ui.toast({ title: t('hosuto.createFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  const loaderOptions: SegmentedOption<Loader>[] = LOADERS.map((l) => ({ value: l, label: l }));

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={t('hosuto.createTitle')}
      size="md"
      footer={
        <Stack direction="row" justify="end" gap={2}>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            {t('hosuto.cancel')}
          </Button>
          <Button variant="primary" onClick={create} loading={busy} disabled={unsupported}>
            {t('hosuto.create')}
          </Button>
        </Stack>
      }
    >
      <Stack gap={4}>
        <Field label={t('hosuto.name')}>
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={slug || t('hosuto.namePlaceholder')} autoFocus />
        </Field>

        <Field
          label={t('hosuto.address')}
          error={slugBad ? t('hosuto.slugRule') : undefined}
          hint={!slugBad && slug && zone ? `${slug}.${zone}` : undefined}
        >
          <Input
            value={slug}
            onChange={(e) => setSlug(e.target.value.toLowerCase())}
            placeholder={t('hosuto.slugPlaceholder')}
            invalid={slugBad}
          />
        </Field>

        <Field label={t('hosuto.mcVersion')}>
          <Autocomplete
            value={mcVersion}
            onChange={setMcVersion}
            onSearch={searchVersions}
            onSelect={(o) => setMcVersion(o.label)}
            placeholder={latest || t('hosuto.loading')}
            minChars={1}
          />
        </Field>

        <Field label={t('hosuto.loader')}>
          <Stack direction="row">
            <SegmentedControl
              value={loader}
              onChange={(v) => {
                setLoader(v);
                setLoaderVersion('');
              }}
              options={loaderOptions}
            />
          </Stack>
        </Field>

        {loader !== 'vanilla' && (
          <Field
            label={t('hosuto.loaderVersion')}
            error={unsupported ? t('hosuto.loaderUnsupported', { loader, mc }) : undefined}
          >
            <Autocomplete
              value={loaderVersion}
              onChange={setLoaderVersion}
              onSearch={searchLoaderVersions}
              onSelect={(o) => setLoaderVersion(o.label)}
              placeholder={loaderVersions[0] ?? t('hosuto.latest')}
              minChars={1}
            />
          </Field>
        )}

        <Field label={t('hosuto.memory')}>
          <Input type="number" min={512} step={512} value={heap} onChange={(e) => setHeap(e.target.value)} />
        </Field>
      </Stack>
    </Modal>
  );
}
