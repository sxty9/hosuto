// Modding owns the (version, loader) pair and the mod set — and they are one subject, not two: a
// mod jar is built for exactly one pair, so changing the pair re-resolves every mod. The daemon
// does that and reports what it had to drop; this surface repeats the loss out loud rather than
// letting the player find out from a crash on boot.
import { useCallback, useEffect, useState } from 'react';
import {
  Avatar,
  Badge,
  Button,
  Field,
  Autocomplete,
  Modal,
  Panel,
  SearchField,
  SegmentedControl,
  Spinner,
  Stack,
  Text,
  TrashIcon,
  IconButton,
  useLiveQuery,
  useT,
  type AutocompleteOption,
  type SegmentedOption,
  type ServiceContextProps,
} from '@holistic/ui';
import {
  LOADERS,
  loaderHasClientMods,
  type CatalogLoadersResp,
  type CatalogModsResp,
  type CatalogVersionsResp,
  type Loader,
  type ModHit,
  type ModsResp,
  type ServerView,
  type VersionResp,
} from './types';

export function Modding({
  api,
  ui,
  srv,
  onChanged,
}: Pick<ServiceContextProps, 'api' | 'ui'> & { srv: ServerView; onChanged: () => void }) {
  const t = useT();
  const q = useLiveQuery<ModsResp>(() => api.get<ModsResp>(`servers/${srv.id}/mods`), 20000, [srv.id]);
  const [changing, setChanging] = useState(false);

  const mods = q.data?.mods ?? [];
  const hasClientMods = q.data?.hasClientMods ?? loaderHasClientMods(srv.loader);

  async function removeMod(id: string, name: string) {
    const ok = await ui.confirm({ title: t('hosuto.removeModTitle', { name }), danger: true, confirmLabel: t('hosuto.remove') });
    if (!ok) return;
    try {
      await api.del(`servers/${srv.id}/mods/${id}`);
      ui.toast({ title: t('hosuto.modRemoved'), variant: 'success' });
      q.refresh();
    } catch (e) {
      ui.toast({ title: t('hosuto.actionFailed'), description: (e as Error).message, variant: 'error' });
    }
  }

  return (
    <Stack gap={4}>
      <Panel
        title={t('hosuto.version')}
        className="p-4"
        actions={
          <Button variant="secondary" size="sm" onClick={() => setChanging(true)}>
            {t('hosuto.changeVersion')}
          </Button>
        }
      >
        <Stack direction="row" align="center" gap={2}>
          <Text weight="semibold">{srv.mcVersion}</Text>
          <Badge variant="neutral">{srv.loader}</Badge>
          {srv.loaderVersion && (
            <Text variant="footnote" color="tertiary">
              {srv.loaderVersion}
            </Text>
          )}
        </Stack>
      </Panel>

      {/* Paper runs Bukkit PLUGINS — server-side only. Vanilla runs nothing. In both cases there is
          no mod list to keep and nothing to hand a player, and saying so is the whole answer. */}
      {!hasClientMods ? (
        <Panel title={t('hosuto.mods')} className="p-4">
          <Text color="secondary">{srv.loader === 'paper' ? t('hosuto.paperPlugins') : t('hosuto.vanillaNoMods')}</Text>
        </Panel>
      ) : (
        <>
          <Panel title={t('hosuto.installed')} className="p-4">
            {q.loading && !q.data ? (
              <Spinner />
            ) : mods.length === 0 ? (
              <Text color="secondary">{t('hosuto.noMods')}</Text>
            ) : (
              <Stack gap={2}>
                {mods.map((m) => (
                  <Stack key={m.id} direction="row" align="center" justify="between" gap={2}>
                    <Stack direction="row" align="center" gap={2}>
                      <Text truncate>{m.name}</Text>
                      {m.clientSide === 'unsupported' && <Badge variant="neutral">{t('hosuto.serverOnly')}</Badge>}
                    </Stack>
                    <IconButton label={t('hosuto.remove')} variant="ghost" onClick={() => void removeMod(m.id, m.name)}>
                      <TrashIcon />
                    </IconButton>
                  </Stack>
                ))}
              </Stack>
            )}
          </Panel>

          <ModSearch api={api} ui={ui} srv={srv} installed={mods.map((m) => m.projectId ?? '')} onAdded={q.refresh} />
        </>
      )}

      {changing && (
        <VersionModal
          api={api}
          ui={ui}
          srv={srv}
          onClose={() => setChanging(false)}
          onSaved={() => {
            q.refresh();
            onChanged();
          }}
        />
      )}
    </Stack>
  );
}

function ModSearch({
  api,
  ui,
  srv,
  installed,
  onAdded,
}: Pick<ServiceContextProps, 'api' | 'ui'> & {
  srv: ServerView;
  installed: string[];
  onAdded: () => void;
}) {
  const t = useT();
  const [query, setQuery] = useState('');
  const [hits, setHits] = useState<ModHit[]>([]);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState('');

  // Debounced: Modrinth is a courtesy, and hammering it on every keystroke is how a service gets
  // rate-limited for everyone on the host.
  useEffect(() => {
    const q = query.trim();
    if (q.length < 2) {
      setHits([]);
      return;
    }
    let live = true;
    setLoading(true);
    const timer = setTimeout(() => {
      api
        .get<CatalogModsResp>(
          `catalog/mods?q=${encodeURIComponent(q)}&mcVersion=${encodeURIComponent(srv.mcVersion)}&loader=${encodeURIComponent(srv.loader)}`,
        )
        .then((r) => live && setHits(r.mods ?? []))
        .catch(() => live && setHits([]))
        .finally(() => live && setLoading(false));
    }, 350);
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [api, query, srv.mcVersion, srv.loader]);

  async function add(hit: ModHit) {
    setBusy(hit.projectId);
    try {
      await api.post(`servers/${srv.id}/mods`, { projectId: hit.projectId });
      ui.toast({ title: t('hosuto.modAdded', { name: hit.title }), variant: 'success' });
      onAdded();
    } catch (e) {
      ui.toast({ title: t('hosuto.modFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy('');
    }
  }

  return (
    <Panel title={t('hosuto.mods')} className="p-4">
      <Stack gap={3}>
        <SearchField value={query} onChange={setQuery} placeholder={t('hosuto.searchMods')} />
        {loading && <Spinner className="h-4 w-4" />}
        {hits.map((h) => {
          const have = installed.includes(h.projectId);
          // A mod the server cannot run must never be installed on it — the daemon refuses, and
          // offering the button anyway would just be a lie with a spinner.
          const clientOnly = h.serverSide === 'unsupported';
          return (
            <Stack key={h.projectId} direction="row" align="center" justify="between" gap={3}>
              <Stack direction="row" align="center" gap={2}>
                <Avatar name={h.title} src={h.iconUrl} size={28} />
                <Stack gap={0}>
                  <Text truncate>{h.title}</Text>
                  <Text variant="caption" color="tertiary" truncate>
                    {h.description}
                  </Text>
                </Stack>
              </Stack>
              {clientOnly ? (
                <Badge variant="neutral">{t('hosuto.clientOnly')}</Badge>
              ) : (
                <Button
                  variant="secondary"
                  size="sm"
                  disabled={have}
                  loading={busy === h.projectId}
                  onClick={() => void add(h)}
                >
                  {have ? t('hosuto.installed') : t('hosuto.add')}
                </Button>
              )}
            </Stack>
          );
        })}
      </Stack>
    </Panel>
  );
}

function VersionModal({
  api,
  ui,
  srv,
  onClose,
  onSaved,
}: Pick<ServiceContextProps, 'api' | 'ui'> & { srv: ServerView; onClose: () => void; onSaved: () => void }) {
  const t = useT();
  const [mcVersion, setMcVersion] = useState('');
  const [loader, setLoader] = useState<Loader>(srv.loader);
  const [loaderVersion, setLoaderVersion] = useState('');
  const [busy, setBusy] = useState(false);
  const [releases, setReleases] = useState<string[]>([]);
  const [loaderVersions, setLoaderVersions] = useState<string[]>([]);

  useEffect(() => {
    api
      .get<CatalogVersionsResp>('catalog/versions')
      .then((r) => setReleases((r.versions ?? []).filter((v) => v.type === 'release').map((v) => v.id)))
      .catch(() => setReleases([]));
  }, [api]);

  // Empty means "keep what the server has" — never a value we invent.
  const mc = mcVersion.trim() || srv.mcVersion;

  useEffect(() => {
    if (loader === 'vanilla') {
      setLoaderVersions([]);
      return;
    }
    let live = true;
    api
      .get<CatalogLoadersResp>(`catalog/loaders?loader=${encodeURIComponent(loader)}&mcVersion=${encodeURIComponent(mc)}`)
      .then((r) => live && setLoaderVersions(r.versions ?? []))
      .catch(() => live && setLoaderVersions([]));
    return () => {
      live = false;
    };
  }, [api, loader, mc]);

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

  async function save() {
    setBusy(true);
    try {
      const r = await api.put<VersionResp>(`servers/${srv.id}/version`, {
        mcVersion: mc,
        loader,
        loaderVersion: loaderVersion.trim(),
      });
      // The dropped list is the point of this call's response: those mods had no build for the new
      // pair and are gone. Naming them is the difference between a change and a surprise.
      ui.toast({
        title: t('hosuto.versionSaved'),
        description: r.dropped.length > 0 ? t('hosuto.dropped', { names: r.dropped.join(', ') }) : undefined,
        variant: r.dropped.length > 0 ? 'info' : 'success',
      });
      onSaved();
      onClose();
    } catch (e) {
      ui.toast({ title: t('hosuto.versionFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  const loaderOptions: SegmentedOption<Loader>[] = LOADERS.map((l) => ({ value: l, label: l }));

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={t('hosuto.changeVersionTitle')}
      size="md"
      footer={
        <Stack direction="row" justify="end" gap={2}>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            {t('hosuto.cancel')}
          </Button>
          <Button variant="primary" onClick={save} loading={busy}>
            {t('hosuto.save')}
          </Button>
        </Stack>
      }
    >
      <Stack gap={4}>
        <Field label={t('hosuto.mcVersion')}>
          <Autocomplete
            value={mcVersion}
            onChange={setMcVersion}
            onSearch={searchVersions}
            onSelect={(o) => setMcVersion(o.label)}
            placeholder={srv.mcVersion}
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
          <Field label={t('hosuto.loaderVersion')}>
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

        {/* Changing the loader away from a modded one throws the mod set away — say so before it
            happens, not in a toast afterwards. */}
        {!loaderHasClientMods(loader) && loaderHasClientMods(srv.loader) && (
          <Text variant="footnote" color="warning">
            {t('hosuto.loaderDropsMods')}
          </Text>
        )}
      </Stack>
    </Modal>
  );
}
