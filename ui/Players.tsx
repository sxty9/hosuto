// Players is the contax mapping made visible: who may join, and how they came to be allowed.
//
// Two things live here, and they are deliberately separate:
//
//   GRANTS  — the rules ("Bob", "my Minecraft group"). This is what the owner edits.
//   PLAYERS — the people those rules currently resolve to. This is what the whitelist becomes.
//
// A contax group grant is never flattened into its members: contax owns its groups, and hosuto
// re-resolves them on every join. That is why the picker below cannot simply hand us addresses —
// see the note on expandGroup.
import { useCallback, useRef, useState } from 'react';
import {
  Avatar,
  Badge,
  Box,
  Button,
  ContactPicker,
  EmptyState,
  Field,
  IconButton,
  Modal,
  Panel,
  PlusIcon,
  SegmentedControl,
  Spinner,
  Stack,
  Text,
  UserIcon,
  XIcon,
  useLiveQuery,
  useT,
  type ContactOption,
  type SegmentedOption,
  type ServiceContextProps,
} from '@holistic/ui';
import type { Grant, JoinPolicy, Level, MembersResp, OnlineResp, PolicyResp, ServerView } from './types';
import { faceUrl } from './face';

interface PickedGroup {
  id: string;
  name: string;
  memberCount: number;
}

export function Players({
  api,
  apiFor,
  ui,
  srv,
}: Pick<ServiceContextProps, 'api' | 'apiFor' | 'ui'> & { srv: ServerView }) {
  const t = useT();
  const q = useLiveQuery<MembersResp>(() => api.get<MembersResp>(`servers/${srv.id}/members`), 15000, [srv.id]);
  // Who is actually connected right now, read live from the server console (every 8s).
  const onlineQ = useLiveQuery<OnlineResp>(() => api.get<OnlineResp>(`servers/${srv.id}/players/online`), 8000, [srv.id]);
  const [adding, setAdding] = useState(false);
  const [busy, setBusy] = useState(false);

  const policy: JoinPolicy = q.data?.policy ?? srv.joinPolicy;
  const grants = q.data?.grants ?? [];
  const players = q.data?.players ?? [];
  const online = onlineQ.data?.online ?? [];
  // Members currently online, keyed by their holistic username, so a member row can show a live dot.
  const onlineUsers = new Set(online.map((o) => o.user).filter(Boolean));

  const policyOptions: SegmentedOption<JoinPolicy>[] = [
    { value: 'whitelist', label: t('hosuto.policyWhitelist') },
    { value: 'open', label: t('hosuto.policyOpen') },
  ];

  async function setPolicy(next: JoinPolicy) {
    if (next === policy) return;
    setBusy(true);
    try {
      const r = await api.put<PolicyResp>(`servers/${srv.id}/policy`, { joinPolicy: next });
      // white-list is read at startup, so the daemon tells us plainly that a running server has
      // not picked the change up yet. Passing that on beats pretending it took effect.
      ui.toast({
        title: t('hosuto.policySaved'),
        description: r.restartRequired ? t('hosuto.restartRequired') : undefined,
        variant: 'success',
      });
      q.refresh();
    } catch (e) {
      ui.toast({ title: t('hosuto.actionFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  async function removeGrant(g: Grant) {
    const ok = await ui.confirm({
      title: t('hosuto.removeGrantTitle', { label: g.label || g.ref }),
      danger: true,
      confirmLabel: t('hosuto.remove'),
    });
    if (!ok) return;
    try {
      await api.del(`servers/${srv.id}/members/${g.id}`);
      ui.toast({ title: t('hosuto.grantRemoved'), variant: 'success' });
      q.refresh();
    } catch (e) {
      ui.toast({ title: t('hosuto.actionFailed'), description: (e as Error).message, variant: 'error' });
    }
  }

  return (
    <Stack gap={4}>
      <Panel title={t('hosuto.policy')} className="p-4">
        <Stack direction="row">
          <SegmentedControl value={policy} onChange={(v) => void setPolicy(v)} options={policyOptions} />
          {busy && <Spinner className="ml-2 h-4 w-4" />}
        </Stack>
      </Panel>

      <Panel title={online.length ? `${t('hosuto.onlineNow')} (${online.length})` : t('hosuto.onlineNow')} className="p-4">
        {!onlineQ.data && onlineQ.loading ? (
          <Spinner className="h-4 w-4" />
        ) : online.length === 0 ? (
          <Text color="secondary">{t('hosuto.nobodyOnline')}</Text>
        ) : (
          <Stack gap={2}>
            {online.map((o) => (
              <Stack key={o.name} direction="row" align="center" gap={2}>
                <Box className="h-2 w-2 shrink-0 rounded-full bg-success" />
                <Avatar name={o.name} src={o.user ? faceUrl(api, o.user, 56) : undefined} size={28} />
                <Text>{o.name}</Text>
              </Stack>
            ))}
          </Stack>
        )}
      </Panel>

      <Panel
        title={t('hosuto.access')}
        className="p-4"
        actions={
          <Button variant="secondary" size="sm" iconLeft={<PlusIcon />} onClick={() => setAdding(true)}>
            {t('hosuto.addPlayer')}
          </Button>
        }
      >
        {grants.length === 0 ? (
          <Text color="secondary">{t('hosuto.noGrants')}</Text>
        ) : (
          <Stack gap={2}>
            {grants.map((g) => (
              <Stack key={g.id} direction="row" align="center" justify="between" gap={2}>
                <Stack direction="row" align="center" gap={2}>
                  <Text>{g.label || g.ref}</Text>
                  <Badge variant="neutral">{t(`hosuto.kind.${g.kind}`)}</Badge>
                  {g.level === 'op' && <Badge variant="accent">{t('hosuto.levelOp')}</Badge>}
                </Stack>
                <IconButton label={t('hosuto.remove')} variant="ghost" onClick={() => void removeGrant(g)}>
                  <XIcon />
                </IconButton>
              </Stack>
            ))}
          </Stack>
        )}
      </Panel>

      <Panel title={t('hosuto.players')} className="p-4">
        {q.loading && !q.data ? (
          <Spinner />
        ) : players.length === 0 ? (
          <EmptyState icon={<UserIcon />} title={t('hosuto.noMembers')} />
        ) : (
          <Stack gap={2}>
            {players.map((p) => (
              <Stack key={p.user} direction="row" align="center" justify="between" gap={2}>
                <Stack direction="row" align="center" gap={2}>
                  <Avatar
                    name={p.name || p.user}
                    src={p.hasAccount ? faceUrl(api, p.user, 56) : undefined}
                    size={28}
                  />
                  <Stack gap={0}>
                    <Text>{p.name || p.user}</Text>
                    {!p.hasAccount && (
                      <Text variant="caption" color="warning">
                        {t('hosuto.noAccount')}
                      </Text>
                    )}
                  </Stack>
                </Stack>
                <Stack direction="row" align="center" gap={2}>
                  {onlineUsers.has(p.user) && (
                    <Stack direction="row" align="center" gap={1}>
                      <Box className="h-2 w-2 shrink-0 rounded-full bg-success" />
                      <Text variant="caption" color="secondary">
                        {t('hosuto.online')}
                      </Text>
                    </Stack>
                  )}
                  <Badge variant={p.level === 'op' ? 'accent' : 'neutral'}>{t(`hosuto.level.${p.level}`)}</Badge>
                </Stack>
              </Stack>
            ))}
          </Stack>
        )}
      </Panel>

      {adding && (
        <AddMembersModal
          api={api}
          apiFor={apiFor}
          ui={ui}
          serverId={srv.id}
          onClose={() => setAdding(false)}
          onAdded={q.refresh}
        />
      )}
    </Stack>
  );
}

function AddMembersModal({
  api,
  apiFor,
  ui,
  serverId,
  onClose,
  onAdded,
}: Pick<ServiceContextProps, 'api' | 'apiFor' | 'ui'> & {
  serverId: string;
  onClose: () => void;
  onAdded: () => void;
}) {
  const t = useT();
  const [people, setPeople] = useState<ContactOption[]>([]);
  const [groups, setGroups] = useState<PickedGroup[]>([]);
  const [level, setLevel] = useState<Level>('play');
  const [busy, setBusy] = useState(false);

  // Group metadata seen during search, so a picked group keeps its name without a second lookup.
  const seen = useRef(new Map<string, PickedGroup>());

  // The directory is contax's, exactly as in icaly: it resolves who this user may address, so an
  // owner adds a person by name instead of by Linux username. contax's own visibility rule is what
  // makes the result safe — and the daemon re-checks it anyway (api.canAdd).
  const searchContacts = useCallback(
    async (query: string): Promise<ContactOption[]> => {
      try {
        const res = await apiFor('contax').get<{
          contacts: ContactOption[];
          groups?: { id: string; name: string; memberCount: number }[];
        }>(`lookup?q=${encodeURIComponent(query)}&includeGroups=1`);
        const gs: ContactOption[] = (res.groups ?? []).map((g) => {
          seen.current.set(g.id, { id: g.id, name: g.name, memberCount: g.memberCount });
          return {
            email: '',
            displayName: g.name,
            kind: 'group' as const,
            groupId: g.id,
            memberCount: g.memberCount,
          };
        });
        return [...gs, ...(res.contacts ?? [])];
      } catch {
        return [];
      }
    },
    [apiFor],
  );

  // ContactPicker expands a picked group into member ADDRESSES and drops the group itself — right
  // for mail, wrong here: a contax grant must stay a group, because hosuto re-resolves its members
  // live on every join (someone removed from the group loses access without anyone touching this
  // server). So the expansion hook is where we capture the group's identity, and it deliberately
  // contributes no addresses; the picked groups are shown as their own chips below.
  const expandGroup = useCallback(async (groupId: string): Promise<ContactOption[]> => {
    const meta = seen.current.get(groupId) ?? { id: groupId, name: groupId, memberCount: 0 };
    setGroups((gs) => (gs.some((g) => g.id === groupId) ? gs : [...gs, meta]));
    return [];
  }, []);

  const levelOptions: SegmentedOption<Level>[] = [
    { value: 'play', label: t('hosuto.level.play') },
    { value: 'op', label: t('hosuto.level.op') },
  ];

  async function submit() {
    // Only members of this holistic instance can be whitelisted: the key hosuto needs is the Linux
    // username, which contax populates for internal users and nobody else has. An external contact
    // is a real contact — it just cannot be a player here.
    const outsiders = people.filter((p) => !p.username);
    if (outsiders.length > 0) {
      ui.toast({
        title: t('hosuto.notMembers', { who: outsiders.map((o) => o.displayName || o.email).join(', ') }),
        variant: 'error',
      });
      return;
    }
    if (people.length === 0 && groups.length === 0) {
      ui.toast({ title: t('hosuto.nobodyPicked'), variant: 'error' });
      return;
    }

    setBusy(true);
    try {
      if (people.length > 0) {
        await api.post(`servers/${serverId}/members`, {
          kind: 'adhoc',
          ref: '',
          label: people.map((p) => p.displayName || p.username).join(', '),
          level,
          members: people.map((p) => p.username),
        });
      }
      for (const g of groups) {
        await api.post(`servers/${serverId}/members`, {
          kind: 'contax',
          ref: g.id,
          label: g.name,
          level,
          members: [],
        });
      }
      ui.toast({ title: t('hosuto.memberAdded'), variant: 'success' });
      onAdded();
      onClose();
    } catch (e) {
      ui.toast({ title: t('hosuto.addFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={t('hosuto.addPlayer')}
      size="md"
      footer={
        <Stack direction="row" justify="end" gap={2}>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            {t('hosuto.cancel')}
          </Button>
          <Button variant="primary" onClick={submit} loading={busy}>
            {t('hosuto.add')}
          </Button>
        </Stack>
      }
    >
      <Stack gap={4}>
        <Field label={t('hosuto.people')}>
          <ContactPicker
            value={people}
            onChange={setPeople}
            onSearch={searchContacts}
            onExpandGroup={expandGroup}
            placeholder={t('hosuto.pickPeople')}
            // A raw email address is nobody hosuto can whitelist — only picked members are.
            allowFreeText={false}
          />
        </Field>

        {/* The picked groups, which the picker itself never shows (it deals in addresses). */}
        {groups.length > 0 && (
          <Stack direction="row" gap={2} wrap>
            {groups.map((g) => (
              <Stack key={g.id} direction="row" align="center" gap={1}>
                <Badge variant="accent">{g.name}</Badge>
                <IconButton
                  label={t('hosuto.remove')}
                  variant="ghost"
                  size="sm"
                  onClick={() => setGroups((gs) => gs.filter((x) => x.id !== g.id))}
                >
                  <XIcon />
                </IconButton>
              </Stack>
            ))}
          </Stack>
        )}

        <Field label={t('hosuto.level')}>
          <Stack direction="row">
            <SegmentedControl value={level} onChange={setLevel} options={levelOptions} />
          </Stack>
        </Field>
      </Stack>
    </Modal>
  );
}
