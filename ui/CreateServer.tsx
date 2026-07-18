// Creating a server, in all three of its forms — blank, from a template, or migrated in from
// somewhere else.
//
// They live behind ONE door on purpose. "New server", "new server from a template" and "bring my
// server over from GPortal" are the same intent with three sources, and splitting them into
// sibling buttons would be exactly the kind of near-identical pair the architecture forbids. So the
// modal opens on a source picker and the rest of the form follows from it.
//
// A migration answers with a JOB rather than a server, because it moves gigabytes: the daemon starts
// the work and the browser watches it. That is the only structural difference between the three
// paths, and JobProgress below is where it is absorbed.
import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Badge,
  Button,
  Checkbox,
  ContextMenu,
  EmptyState,
  Field,
  Autocomplete,
  Input,
  Modal,
  Panel,
  PasswordInput,
  ProgressBar,
  SegmentedControl,
  ServerIcon,
  Stack,
  Text,
  UploadControl,
  cn,
  useT,
  type AutocompleteOption,
  type MenuItem,
  type SegmentedOption,
  type ServiceContextProps,
  type TranslateFn,
} from '@holistic/ui';
import {
  LOADERS,
  type CatalogLoadersResp,
  type CatalogVersionsResp,
  type CreateMode,
  type Info,
  type Job,
  type Loader,
  type MigrateSource,
  type ServerView,
  type Template,
  type TemplatesResp,
} from './types';

// Mirrors store.SlugRe. The slug is a public DNS label, so the rule is the daemon's, not ours —
// checking it here only saves a round trip, it never decides anything.
const SLUG_RE = /^[a-z][a-z0-9-]{1,31}$/;

type Props = Pick<ServiceContextProps, 'api' | 'ui'> & {
  onClose: () => void;
  onCreated: (id: string) => void;
};

export function CreateServerModal({ api, ui, onClose, onCreated }: Props) {
  const t = useT();
  const [mode, setMode] = useState<CreateMode>('new');

  // Name and address are asked once, whatever the source: they are facts about the server being
  // made here, not about where its contents come from.
  const [name, setName] = useState('');
  const [slug, setSlug] = useState('');
  const [zone, setZone] = useState('');
  const [busy, setBusy] = useState(false);
  const [job, setJob] = useState<Job | null>(null);

  // new
  const [mcVersion, setMcVersion] = useState('');
  const [loader, setLoader] = useState<Loader>('fabric');
  const [loaderVersion, setLoaderVersion] = useState('');
  const [heap, setHeap] = useState('2048');
  const [releases, setReleases] = useState<string[]>([]);
  const [loaderVersions, setLoaderVersions] = useState<string[]>([]);
  const [loadersLoading, setLoadersLoading] = useState(false);

  // template
  const [templates, setTemplates] = useState<Template[]>([]);
  const [templateId, setTemplateId] = useState('');

  // migrate
  const [source, setSource] = useState<MigrateSource>('upload');
  const [file, setFile] = useState<File | null>(null);
  const [host, setHost] = useState('');
  const [port, setPort] = useState('21');
  const [user, setUser] = useState('');
  const [pass, setPass] = useState('');
  const [remotePath, setRemotePath] = useState('');

  const loadTemplates = useCallback(() => {
    api
      .get<TemplatesResp>('templates')
      .then((r) => setTemplates(r.templates ?? []))
      .catch(() => setTemplates([]));
  }, [api]);

  // The catalogue is the daemon's, not ours: versions come from Mojang's manifest and loader
  // builds from each loader's own index, both proxied and cached by the backend.
  useEffect(() => {
    api.get<Info>('info').then((i) => setZone(i.zone)).catch(() => setZone(''));
    api
      .get<CatalogVersionsResp>('catalog/versions')
      .then((r) => setReleases((r.versions ?? []).filter((v) => v.type === 'release').map((v) => v.id)))
      .catch(() => setReleases([]));
    loadTemplates();
  }, [api, loadTemplates]);

  const latest = releases[0] ?? '';
  const mc = mcVersion.trim() || latest;

  // Pre-fill the newest release once the catalogue loads, so the field shows the value that will
  // actually be used instead of a gray hint the user has to trust is adopted. Only an untouched
  // field is filled — once they type, their choice stands.
  useEffect(() => {
    if (!mcVersion && releases.length > 0) setMcVersion(releases[0]);
  }, [releases, mcVersion]);

  useEffect(() => {
    if (mode !== 'new' || loader === 'vanilla' || !mc) {
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
  }, [api, loader, mc, mode]);

  useEffect(() => {
    if (loader !== 'vanilla' && !loaderVersion && loaderVersions.length > 0) {
      setLoaderVersion(loaderVersions[0]);
    }
  }, [loaderVersions, loader, loaderVersion]);

  // Mod loaders lag weeks behind a Minecraft release, so "newest Minecraft + NeoForge" — the most
  // natural thing to pick, and what this form defaults to — often has no builds at all. Say so at
  // the field instead of letting the user press Create and collect a failure.
  const unsupported = mode === 'new' && loader !== 'vanilla' && !!mc && !loadersLoading && loaderVersions.length === 0;

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
  // Set once the user has actually tried to submit, so an empty required field is marked AT the
  // field. A toast alone cannot say which field it means — and this form has two that take an
  // address-shaped value, so "which one" is the entire question.
  const [tried, setTried] = useState(false);
  const slugMissing = tried && !slug;

  async function removeTemplate(tpl: Template) {
    const ok = await ui.confirm({
      title: t('hosuto.tpl.deleteTitle', { name: tpl.name }),
      danger: true,
      confirmLabel: t('hosuto.delete'),
    });
    if (!ok) return;
    try {
      await api.del(`templates/${tpl.id}`);
      if (templateId === tpl.id) setTemplateId('');
      loadTemplates();
    } catch (e) {
      ui.toast({ title: t('hosuto.actionFailed'), description: (e as Error).message, variant: 'error' });
    }
  }

  async function submit() {
    setTried(true);
    if (!slug || slugBad) {
      ui.toast({ title: slug ? t('hosuto.slugRule') : t('hosuto.mig.needSlug'), variant: 'error' });
      return;
    }
    if (mode === 'new' && !mc) {
      ui.toast({ title: t('hosuto.pickVersion'), variant: 'error' });
      return;
    }
    if (mode === 'template' && !templateId) {
      ui.toast({ title: t('hosuto.tpl.pick'), variant: 'error' });
      return;
    }
    if (mode === 'migrate' && source === 'upload' && !file) {
      ui.toast({ title: t('hosuto.mig.pickFile'), variant: 'error' });
      return;
    }
    if (mode === 'migrate' && source === 'ftp' && !host.trim()) {
      ui.toast({ title: t('hosuto.mig.needHost'), variant: 'error' });
      return;
    }

    setBusy(true);
    try {
      if (mode === 'migrate') {
        setJob(await startImport());
        return; // the modal now watches the job; it closes when the job finishes
      }
      const srv = await api.post<ServerView>('servers', {
        name: name.trim() || slug,
        slug,
        ...(mode === 'template'
          ? { templateId }
          : {
              mcVersion: mc,
              loader,
              loaderVersion: loaderVersion.trim(), // empty: the daemon takes the newest build
              heapMB: Number(heap) || 0,
            }),
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

  // An upload has to travel as multipart — the archive is the payload, not a JSON field — so this is
  // the one call that goes through api.raw rather than api.post.
  async function startImport(): Promise<Job> {
    if (source === 'upload' && file) {
      const fd = new FormData();
      fd.append('name', name.trim() || slug);
      fd.append('slug', slug);
      fd.append('archive', file);
      const res = await api.raw('servers/import', { method: 'POST', body: fd });
      const body = await res.json();
      if (!res.ok) throw new Error(body?.error ?? t('hosuto.createFailed'));
      return body as Job;
    }
    return api.post<Job>('servers/import', {
      name: name.trim() || slug,
      slug,
      source: 'ftp',
      host: host.trim(),
      port: Number(port) || 21,
      user: user.trim(),
      pass,
      path: remotePath.trim(),
    });
  }

  // While a migration runs the modal becomes its progress view. Cancelling is offered because the
  // alternative — a multi-gigabyte transfer the user cannot stop — is worse than an aborted one.
  if (job) {
    return (
      <Modal
        open
        onOpenChange={() => {}}
        title={t('hosuto.mig.title')}
        size="md"
        footer={
          <Stack direction="row" justify="end" gap={2}>
            {job.state === 'running' ? (
              <Button variant="ghost" onClick={() => void api.del(`jobs/${job.id}`).catch(() => {})}>
                {t('hosuto.cancel')}
              </Button>
            ) : (
              <Button
                variant="primary"
                onClick={() => {
                  if (job.serverId) onCreated(job.serverId);
                  onClose();
                }}
              >
                {t('hosuto.close')}
              </Button>
            )}
          </Stack>
        }
      >
        <JobProgress
          api={api}
          jobId={job.id}
          t={t}
          onSettled={(j) => {
            setJob(j);
            if (j.state === 'done') ui.toast({ title: t('hosuto.mig.done'), variant: 'success' });
          }}
        />
      </Modal>
    );
  }

  const modeOptions: SegmentedOption<CreateMode>[] = [
    { value: 'new', label: t('hosuto.mode.new') },
    { value: 'template', label: t('hosuto.mode.template') },
    { value: 'migrate', label: t('hosuto.mode.migrate') },
  ];
  const sourceOptions: SegmentedOption<MigrateSource>[] = [
    { value: 'upload', label: t('hosuto.mig.upload') },
    { value: 'ftp', label: t('hosuto.mig.remote') },
  ];
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
          <Button variant="primary" onClick={submit} loading={busy} disabled={unsupported}>
            {mode === 'migrate' ? t('hosuto.mig.start') : t('hosuto.create')}
          </Button>
        </Stack>
      }
    >
      <Stack gap={4}>
        <Stack direction="row">
          <SegmentedControl value={mode} onChange={setMode} options={modeOptions} />
        </Stack>

        <Field label={t('hosuto.name')}>
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={slug || t('hosuto.namePlaceholder')} autoFocus />
        </Field>

        <Field
          label={t('hosuto.address')}
          error={slugBad ? t('hosuto.slugRule') : slugMissing ? t('hosuto.mig.needSlug') : undefined}
          hint={!slugBad && slug && zone ? `${slug}.${zone}` : undefined}
        >
          <Input
            value={slug}
            onChange={(e) => setSlug(e.target.value.toLowerCase())}
            placeholder={t('hosuto.slugPlaceholder')}
            invalid={slugBad || slugMissing}
          />
        </Field>

        {mode === 'new' && (
          <>
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
          </>
        )}

        {mode === 'template' && (
          <TemplatePicker
            templates={templates}
            selected={templateId}
            onSelect={setTemplateId}
            onDelete={removeTemplate}
            t={t}
          />
        )}

        {mode === 'migrate' && (
          <>
            <Stack direction="row">
              <SegmentedControl value={source} onChange={setSource} options={sourceOptions} />
            </Stack>

            {source === 'upload' ? (
              <Field label={t('hosuto.mig.archive')} hint={file ? file.name : undefined}>
                <Stack direction="row">
                  <UploadControl
                    label={file ? t('hosuto.mig.otherFile') : t('hosuto.mig.chooseFile')}
                    variant="secondary"
                    onFiles={(fs) => setFile(fs[0] ?? null)}
                  />
                </Stack>
              </Field>
            ) : (
              <>
                <Stack direction="row" gap={2} align="end">
                  <Field label={t('hosuto.mig.host')} className="flex-1">
                    <Input
                      value={host}
                      onChange={(e) => setHost(e.target.value)}
                      placeholder={t('hosuto.mig.hostPlaceholder')}
                    />
                  </Field>
                  <Field label={t('hosuto.mig.port')} className="w-24">
                    <Input type="number" value={port} onChange={(e) => setPort(e.target.value)} />
                  </Field>
                </Stack>
                <Field label={t('hosuto.mig.user')}>
                  <Input value={user} onChange={(e) => setUser(e.target.value)} autoComplete="off" />
                </Field>
                <Field label={t('hosuto.mig.pass')}>
                  <PasswordInput value={pass} onChange={(e) => setPass(e.target.value)} autoComplete="new-password" />
                </Field>
                <Field label={t('hosuto.mig.path')} hint="/">
                  <Input value={remotePath} onChange={(e) => setRemotePath(e.target.value)} placeholder="/" />
                </Field>
              </>
            )}
          </>
        )}
      </Stack>
    </Modal>
  );
}

// ── the template picker ───────────────────────────────────────────────────────────────

// Templates are shown as rows rather than as a dropdown because a template has facts that decide
// the choice — which Minecraft it is for, and whether it carries a world. A select would hide both
// behind a name.
function TemplatePicker({
  templates,
  selected,
  onSelect,
  onDelete,
  t,
}: {
  templates: Template[];
  selected: string;
  onSelect: (id: string) => void;
  onDelete: (tpl: Template) => void;
  t: TranslateFn;
}) {
  if (templates.length === 0) {
    return <EmptyState icon={<ServerIcon />} title={t('hosuto.tpl.none')} />;
  }
  return (
    <Stack gap={2}>
      {templates.map((tpl) => {
        const items: MenuItem[] = [
          { id: 'use', label: t('hosuto.tpl.use'), onSelect: () => onSelect(tpl.id) },
          { id: 'delete', label: t('hosuto.delete'), danger: true, separatorBefore: true, onSelect: () => onDelete(tpl) },
        ];
        return (
          <ContextMenu key={tpl.id} items={items}>
            <Panel
              interactive
              onClick={() => onSelect(tpl.id)}
              className={cn('cursor-pointer p-3', selected === tpl.id && 'ring-2 ring-accent')}
            >
              <Stack direction="row" align="center" justify="between" gap={3} wrap>
                <Stack gap={0}>
                  <Text weight="semibold">{tpl.name}</Text>
                  <Text variant="caption" color="tertiary">
                    {tpl.loader} {tpl.mcVersion}
                  </Text>
                </Stack>
                <Stack direction="row" align="center" gap={2}>
                  {tpl.includeWorld && <Badge variant="neutral">{t('hosuto.tpl.withWorld')}</Badge>}
                  <Text variant="caption" color="tertiary">
                    {formatSize(tpl.size)}
                  </Text>
                </Stack>
              </Stack>
            </Panel>
          </ContextMenu>
        );
      })}
    </Stack>
  );
}

// ── saving a server as a template ─────────────────────────────────────────────────────

export function SaveTemplateModal({
  api,
  ui,
  srv,
  onClose,
}: Pick<ServiceContextProps, 'api' | 'ui'> & { srv: { id: string; name: string }; onClose: () => void }) {
  const t = useT();
  const [name, setName] = useState(srv.name);
  const [includeWorld, setIncludeWorld] = useState(false);
  const [busy, setBusy] = useState(false);
  const [job, setJob] = useState<Job | null>(null);

  async function save() {
    setBusy(true);
    try {
      setJob(await api.post<Job>('templates', { serverId: srv.id, name: name.trim() || srv.name, includeWorld }));
    } catch (e) {
      ui.toast({ title: t('hosuto.actionFailed'), description: (e as Error).message, variant: 'error' });
      setBusy(false);
    }
  }

  if (job) {
    return (
      <Modal
        open
        onOpenChange={() => {}}
        title={t('hosuto.tpl.saveTitle')}
        size="sm"
        footer={
          <Stack direction="row" justify="end" gap={2}>
            <Button variant="primary" disabled={job.state === 'running'} onClick={onClose}>
              {t('hosuto.close')}
            </Button>
          </Stack>
        }
      >
        <JobProgress
          api={api}
          jobId={job.id}
          t={t}
          onSettled={(j) => {
            setJob(j);
            if (j.state === 'done') ui.toast({ title: t('hosuto.tpl.saved'), variant: 'success' });
          }}
        />
      </Modal>
    );
  }

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={t('hosuto.tpl.saveTitle')}
      size="sm"
      footer={
        <Stack direction="row" justify="end" gap={2}>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            {t('hosuto.cancel')}
          </Button>
          <Button variant="primary" onClick={save} loading={busy}>
            {t('hosuto.tpl.save')}
          </Button>
        </Stack>
      }
    >
      <Stack gap={4}>
        <Field label={t('hosuto.name')}>
          <Input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
        </Field>
        <Checkbox checked={includeWorld} onChange={setIncludeWorld} label={t('hosuto.tpl.includeWorld')} />
      </Stack>
    </Modal>
  );
}

// ── watching a job ────────────────────────────────────────────────────────────────────

// JobProgress polls one job and renders it. It is deliberately the same component for a migration
// and for packing a template: both are "the daemon is doing something that takes a while", and the
// only thing that differs is which phases go by.
export function JobProgress({
  api,
  jobId,
  t,
  onSettled,
}: Pick<ServiceContextProps, 'api'> & { jobId: string; t: TranslateFn; onSettled: (j: Job) => void }) {
  const [job, setJob] = useState<Job | null>(null);

  // onSettled is held in a ref rather than named as a dependency. The parent passes a fresh closure
  // on every render, so depending on it would tear down and restart the poll loop continuously —
  // the ref keeps the effect tied to the thing that actually identifies the work: the job id.
  const settled = useRef(onSettled);
  settled.current = onSettled;

  // Polled rather than streamed: a job's state is a handful of numbers, and the SSE machinery the
  // chat uses would buy nothing here. The loop stops as soon as the job settles.
  useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async () => {
      try {
        const j = await api.get<Job>(`jobs/${jobId}`);
        if (!live) return;
        setJob(j);
        if (j.state !== 'running') {
          settled.current(j);
          return;
        }
      } catch {
        // A transient failure must not abandon a running migration; the next tick tries again.
      }
      if (live) timer = setTimeout(() => void tick(), 1500);
    };
    void tick();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [api, jobId]);

  if (!job) {
    return <ProgressBar indeterminate />;
  }

  const pct = job.total > 0 ? Math.min(100, Math.round((job.done / job.total) * 100)) : undefined;
  const phaseLabel = t.has(`hosuto.job.${job.phase}`) ? t(`hosuto.job.${job.phase}`) : job.phase;

  return (
    <Stack gap={4}>
      <Stack gap={2}>
        <Stack direction="row" align="center" justify="between" gap={2}>
          <Text weight="semibold">{job.state === 'running' ? phaseLabel : t(`hosuto.job.${job.state}`)}</Text>
          {job.state === 'running' && pct !== undefined && (
            <Text variant="caption" color="tertiary">
              {pct}%
            </Text>
          )}
        </Stack>
        {job.state === 'running' && (
          <>
            <ProgressBar value={pct} indeterminate={pct === undefined} />
            {job.message && (
              <Text variant="caption" color="tertiary">
                {job.message}
              </Text>
            )}
          </>
        )}
      </Stack>

      {job.error && <Text color="danger">{job.error}</Text>}

      {/* The notes are the migration's report — what was recognised, what was skipped, and who
          could not be carried across. They are the whole reason a migration is trustworthy, so
          they are shown, not logged. */}
      {job.notes && job.notes.length > 0 && (
        <Stack gap={1}>
          {job.notes.map((n, i) => (
            <Text key={i} variant="caption" color="secondary">
              {n}
            </Text>
          ))}
        </Stack>
      )}
    </Stack>
  );
}

function formatSize(bytes: number): string {
  if (bytes <= 0) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let n = bytes;
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)} ${units[i]}`;
}
