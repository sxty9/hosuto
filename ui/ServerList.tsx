// ServerList is hosuto's root view: the servers you own, and the servers other people added you
// to. The second list is the whole point of the contax mapping — hosuto owns the Linux-user →
// Minecraft-account link, so "Bob added you" can become a whitelist entry without anyone typing a
// UUID. It is also where the account itself gets linked, because without it a member cannot be
// whitelisted anywhere and every other surface here is useless to them.
import { useState } from 'react';
import {
  Avatar,
  Badge,
  Box,
  Button,
  ContextMenu,
  EmptyState,
  Input,
  Panel,
  PlusIcon,
  ServerIcon,
  Spinner,
  Stack,
  Text,
  TrashIcon,
  cn,
  useLiveQuery,
  useT,
  userHasRight,
  type MenuItem,
  type ServiceContextProps,
  type TranslateFn,
} from '@holistic/ui';
import { PairDeviceModal } from './PairDevice';
import { CreateServerModal, SaveTemplateModal } from './CreateServer';
import { type Account, type RunState, type ServerView, type ServersResp } from './types';
import { faceUrl } from './face';

const HOST = 'hp_hosuto_host';
const PLAY = 'hp_hosuto_play';
const ADMIN = 'hp_hosuto_admin';

const DOT: Record<RunState, string> = {
  active: 'bg-success',
  activating: 'bg-warning',
  deactivating: 'bg-warning',
  reloading: 'bg-warning',
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
                  canTemplate={canHost}
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
                  // A template carries the source server's config files, so making one is an
                  // owner-or-admin act — the same gate the daemon applies.
                  canTemplate={canAdmin && canHost}
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
  const [pairing, setPairing] = useState(false);

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

  // Linked: stay out of the way — a name, a face, a way to bring the desktop app along, and a way back
  // out. Pairing lives here because a paired device is a fact about the ACCOUNT, not about any one
  // server: the token it mints reaches every server the player can already reach.
  if (account) {
    return (
      <>
        <Stack direction="row" align="center" gap={2}>
          <Avatar name={account.name} src={faceUrl(api, account.user, 48)} size={24} />
          <Text variant="footnote" color="secondary">
            {account.name}
          </Text>
          <Button variant="ghost" size="sm" onClick={() => setPairing(true)}>
            {t('hosuto.pairDevice')}
          </Button>
          <Button variant="ghost" size="sm" onClick={unlink}>
            {t('hosuto.unlink')}
          </Button>
        </Stack>
        {pairing && <PairDeviceModal api={api} ui={ui} onClose={() => setPairing(false)} />}
      </>
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
  canTemplate,
  onOpen,
  onChanged,
}: Pick<ServiceContextProps, 'api' | 'ui'> & {
  srv: ServerView;
  t: TranslateFn;
  canControl: boolean;
  canDelete: boolean;
  canTemplate: boolean;
  onOpen: (id: string) => void;
  onChanged: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [templating, setTemplating] = useState(false);
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
  // Saving a template lives on the server itself — a template is made FROM a server, so the act
  // belongs where the server is, not behind a separate "templates" screen nobody would find.
  if (canTemplate) {
    items.push({
      id: 'template',
      label: t('hosuto.tpl.save'),
      separatorBefore: true,
      onSelect: () => setTemplating(true),
    });
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
    <>
      {templating && (
        <SaveTemplateModal api={api} ui={ui} srv={srv} onClose={() => setTemplating(false)} />
      )}
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
    </>
  );
}
