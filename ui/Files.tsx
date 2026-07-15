// Files is the "Spieledateien" tab: the holistic Files browser, pointed at ONE server's on-disk
// tree. It is deliberately the same components the samba Files service uses — FileBrowser,
// FileToolbar, FilePreview, the dialogs — because the Reuse-before-Build maxim says a file explorer
// is not something hosuto reinvents. Only the glue is local: every fs call is scoped to this server
// (servers/<id>/fs/...), and there is a single virtual root, "server", instead of samba's drives.
import { useCallback, useEffect, useState } from 'react';
import {
  Breadcrumb,
  Button,
  CopyIcon,
  FileBrowser,
  FilePreview,
  FileToolbar,
  MoveIcon,
  NewFolderDialog,
  Panel,
  RenameDialog,
  Stack,
  Text,
  UploadControl,
  formatBytes,
  formatDate,
  useT,
  type BreadcrumbSegment,
  type FileActionId,
  type FileEntry,
  type FileThumbSources,
  type ServiceContextProps,
  type TextPayload,
} from '@holistic/ui';
import type { ServerView } from './types';

interface Clipboard {
  mode: 'move' | 'copy';
  items: FileEntry[];
}

const q = (path: string) => encodeURIComponent(path);
const parentOf = (path: string) => path.split('/').slice(0, -1).join('/');

function buildBreadcrumb(cwd: string, rootLabel: string): BreadcrumbSegment[] {
  const [root, ...rest] = cwd.split('/');
  const segs: BreadcrumbSegment[] = [{ label: rootLabel, path: root }];
  let acc = root;
  for (const part of rest) {
    if (!part) continue;
    acc += '/' + part;
    segs.push({ label: part, path: acc });
  }
  return segs;
}

export function Files({ user, api, apiFor, ui, nav, srv }: Pick<ServiceContextProps, 'user' | 'api' | 'apiFor' | 'ui' | 'nav'> & { srv: ServerView }) {
  const t = useT();
  // Every fs endpoint hangs under this server, so the whole component talks through one prefix.
  const base = `servers/${srv.id}/fs`;

  const [cwd, setCwd] = useState('server');
  const [entries, setEntries] = useState<FileEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [view, setView] = useState<'grid' | 'list'>('list');
  const [search, setSearch] = useState('');
  const [selection, setSelection] = useState<Set<string>>(new Set());
  const [preview, setPreview] = useState<{ entry: FileEntry; rawUrl?: string; text?: TextPayload | null } | null>(null);
  const [renaming, setRenaming] = useState<FileEntry | null>(null);
  const [newFolderOpen, setNewFolderOpen] = useState(false);
  const [clipboard, setClipboard] = useState<Clipboard | null>(null);

  const reload = useCallback(() => {
    setLoading(true);
    setError(null);
    setSelection(new Set());
    api
      .get<{ path: string; entries: FileEntry[] }>(`${base}/list?path=${q(cwd)}`)
      .then((r) => setEntries(r.entries))
      .catch((e: Error) => setError(e.message || t('hosuto.files.loadError')))
      .finally(() => setLoading(false));
  }, [api, base, cwd, t]);

  useEffect(() => reload(), [reload]);

  const canGoUp = cwd.includes('/'); // false at the server root
  const filtered = search ? entries.filter((e) => e.name.toLowerCase().includes(search.toLowerCase())) : entries;
  const selectedEntries = entries.filter((e) => selection.has(e.path));
  const viewable = filtered.filter((e) => e.kind === 'file' && !!e.viewer);
  const previewIdx = preview ? viewable.findIndex((e) => e.path === preview.entry.path) : -1;
  const cutPaths = clipboard?.mode === 'move' ? new Set(clipboard.items.map((i) => i.path)) : undefined;

  const thumbnails: FileThumbSources = {
    mediaUrl: (e) => api.url(`${base}/raw?path=${q(e.path)}`),
    loadText: (e) => api.get<TextPayload>(`${base}/text?path=${q(e.path)}`),
  };

  function navigate(path: string) {
    setSearch('');
    setCwd(path);
  }

  function download(items: FileEntry[]) {
    for (const it of items) {
      const a = document.createElement('a');
      a.href = api.url(`${base}/download?path=${q(it.path)}`);
      a.download = it.name;
      document.body.appendChild(a);
      a.click();
      a.remove();
    }
  }

  async function openEntry(entry: FileEntry) {
    if (entry.kind === 'dir') {
      navigate(entry.path);
      return;
    }
    if (entry.viewer === 'text' || entry.viewer === 'markdown') {
      try {
        const payload = await api.get<TextPayload>(`${base}/text?path=${q(entry.path)}`);
        setPreview({ entry, text: payload });
      } catch {
        setPreview({ entry, text: null });
      }
    } else if (entry.viewer) {
      setPreview({ entry, rawUrl: api.url(`${base}/raw?path=${q(entry.path)}`) });
    } else {
      download([entry]);
    }
  }

  async function confirmDelete(targets: FileEntry[]) {
    const label = targets.length > 1 ? t('hosuto.files.items', { count: targets.length }) : `„${targets[0].name}“`;
    const ok = await ui.confirm({ title: t('hosuto.files.deleteTitle', { label }), description: t('hosuto.files.deleteUndo'), danger: true, confirmLabel: t('hosuto.remove') });
    if (!ok) return;
    try {
      for (const target of targets) await api.post(`${base}/delete`, { path: target.path, recursive: target.kind === 'dir' });
      ui.toast({ title: t('hosuto.files.deleted'), variant: 'success' });
      reload();
    } catch (e) {
      ui.toast({ title: t('hosuto.files.deleteFailed'), description: (e as Error).message, variant: 'error' });
    }
  }

  function handleAction(action: FileActionId, targets: FileEntry[]) {
    if (action === 'download') download(targets.filter((x) => x.kind === 'file'));
    else if (action === 'rename') setRenaming(targets[0]);
    else if (action === 'move') {
      setClipboard({ mode: 'move', items: targets });
      setSelection(new Set());
    } else if (action === 'copy') {
      setClipboard({ mode: 'copy', items: targets });
      setSelection(new Set());
    } else if (action === 'delete') void confirmDelete(targets);
    else if (action === 'info') {
      const x = targets[0];
      ui.toast({ title: x.name, description: `${x.kind === 'dir' ? t('hosuto.files.folder') : formatBytes(x.size)} · ${formatDate(x.mtime)}` });
    }
  }

  async function paste() {
    if (!clipboard) return;
    const op = clipboard.mode === 'move' ? 'move' : 'copy';
    let done = 0;
    try {
      for (const it of clipboard.items) {
        if (clipboard.mode === 'move' && parentOf(it.path) === cwd) continue;
        await api.post(`${base}/${op}`, { src: it.path, dstDir: cwd });
        done += 1;
      }
      ui.toast({ title: clipboard.mode === 'move' ? t('hosuto.files.moved') : t('hosuto.files.copied'), description: t('hosuto.files.items', { count: done || clipboard.items.length }), variant: 'success' });
      setClipboard(null);
      reload();
    } catch (e) {
      ui.toast({ title: t('hosuto.files.opFailed'), description: (e as Error).message, variant: 'error' });
    }
  }

  async function doRename(name: string) {
    if (!renaming) return;
    try {
      await api.post(`${base}/rename`, { path: renaming.path, newName: name });
      ui.toast({ title: t('hosuto.files.renamed'), variant: 'success' });
      reload();
    } catch (e) {
      ui.toast({ title: t('hosuto.files.opFailed'), description: (e as Error).message, variant: 'error' });
    }
  }

  async function doMkdir(name: string) {
    try {
      await api.post(`${base}/mkdir`, { path: cwd, name });
      ui.toast({ title: t('hosuto.files.folderCreated'), variant: 'success' });
      reload();
    } catch (e) {
      ui.toast({ title: t('hosuto.files.opFailed'), description: (e as Error).message, variant: 'error' });
    }
  }

  async function doUpload(files: File[]) {
    try {
      for (const f of files) {
        const fd = new FormData();
        fd.append('path', cwd);
        fd.append('file', f);
        const res = await api.raw(`${base}/upload`, { method: 'POST', body: fd });
        if (!res.ok) throw new Error(t('hosuto.files.uploadOneFailed', { name: f.name }));
      }
      ui.toast({ title: t('hosuto.files.uploaded', { count: files.length }), variant: 'success' });
      reload();
    } catch (e) {
      ui.toast({ title: t('hosuto.files.uploadFailed'), description: (e as Error).message, variant: 'error' });
    }
  }

  const clipboardSummary = clipboard
    ? clipboard.items.length === 1
      ? clipboard.items[0].name
      : t('hosuto.files.items', { count: clipboard.items.length })
    : '';

  return (
    <Stack gap={3}>
      <Breadcrumb segments={buildBreadcrumb(cwd, t('hosuto.files.root'))} onNavigate={navigate} />
      <FileToolbar
        view={view}
        onViewChange={setView}
        search={search}
        onSearch={setSearch}
        selection={selectedEntries}
        canWrite
        canGoUp={canGoUp}
        onNavigateUp={() => navigate(parentOf(cwd))}
        onNewFolder={() => setNewFolderOpen(true)}
        onUpload={doUpload}
        onAction={handleAction}
      />

      {clipboard && (
        <Stack direction="row" align="center" justify="between" gap={3} className="rounded-md border border-accent/40 bg-accent/10 px-3 py-2">
          <Text variant="footnote" color="secondary">
            {clipboard.mode === 'move' ? t('hosuto.files.clipboardMove', { what: clipboardSummary }) : t('hosuto.files.clipboardCopy', { what: clipboardSummary })}
          </Text>
          <Stack direction="row" gap={2}>
            <Button variant="ghost" size="sm" onClick={() => setClipboard(null)}>
              {t('hosuto.cancel')}
            </Button>
            <Button variant="primary" size="sm" iconLeft={clipboard.mode === 'move' ? <MoveIcon className="h-4 w-4" /> : <CopyIcon className="h-4 w-4" />} onClick={paste}>
              {clipboard.mode === 'move' ? t('hosuto.files.moveHere') : t('hosuto.files.copyHere')}
            </Button>
          </Stack>
        </Stack>
      )}

      <Panel>
        <FileBrowser
          entries={filtered}
          view={view}
          selection={selection}
          loading={loading}
          error={error}
          cutPaths={cutPaths}
          thumbnails={thumbnails}
          onOpen={openEntry}
          onSelectionChange={setSelection}
          onAction={handleAction}
          emptyAction={<UploadControl onFiles={doUpload} />}
        />
      </Panel>

      <FilePreview
        open={!!preview}
        entry={preview?.entry ?? null}
        rawUrl={preview?.rawUrl}
        text={preview?.text}
        onOpenChange={(o) => !o && setPreview(null)}
        onDownload={(e) => download([e])}
        onPrev={previewIdx > 0 ? () => openEntry(viewable[previewIdx - 1]) : undefined}
        onNext={previewIdx >= 0 && previewIdx < viewable.length - 1 ? () => openEntry(viewable[previewIdx + 1]) : undefined}
        actionHost={{
          apiFor,
          ui,
          user,
          openService: nav.openService,
          loadBytes: preview
            ? async () => {
                const res = await api.raw(`${base}/raw?path=${q(preview.entry.path)}`);
                if (!res.ok) throw new Error(t('hosuto.files.loadError'));
                return new Uint8Array(await res.arrayBuffer());
              }
            : undefined,
        }}
      />
      <NewFolderDialog open={newFolderOpen} onOpenChange={setNewFolderOpen} onSubmit={doMkdir} />
      <RenameDialog open={!!renaming} initialName={renaming?.name ?? ''} onOpenChange={(o) => !o && setRenaming(null)} onSubmit={doRename} />
    </Stack>
  );
}
