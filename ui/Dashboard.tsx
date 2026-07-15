// Dashboard is hosuto's plugin root. It is a two-level surface, not a tab bar:
//
//   ROOT    — the server list (the servers you own, and the servers others added you to).
//   SERVER  — one selected server, with the four tabs (reachability, players, mods, export).
//
// Selection AND tab live in the URL (/app/hosuto/<serverId>/<tab>) via nav, which is the
// contract's own persistence mechanism: a reload restores both, and the address bar names what
// you are looking at. A private localStorage key would hide that state from the shell.
//
// Rights (keep in sync with permissions/hosuto.json and backend internal/rights):
//   hp_hosuto_play  — see joinable servers, link an account, export (default-on)
//   hp_hosuto_host  — create and own servers → the create action and the owner-only tabs
//   hp_hosuto_admin — see and control every server
//
// The tab set mirrors the backend's own gates: the member/policy/mod/version routes are guarded
// by hp_hosuto_host AND then by owner-or-admin, so a non-owner on a joinable server gets exactly
// two tabs — Reachability and Client Export. Offering more would only earn them a 403.
import { useEffect } from 'react';
import {
  Badge,
  Button,
  ChevronLeftIcon,
  ContentRegion,
  EmptyState,
  Heading,
  Panel,
  SegmentedControl,
  ServerIcon,
  Spinner,
  Stack,
  Text,
  useLiveQuery,
  useT,
  userHasRight,
  type SegmentedOption,
  type ServiceContextProps,
} from '@holistic/ui';
import { ServerList } from './ServerList';
import { Reachability } from './Reachability';
import { Players } from './Players';
import { Modding } from './Modding';
import { ClientExport } from './ClientExport';
import { Files } from './Files';
import { AskAi } from './AskAi';
import type { ServerView, Tab } from './types';

const PLAY = 'hp_hosuto_play';
const HOST = 'hp_hosuto_host';
const ADMIN = 'hp_hosuto_admin';
// Ask AI runs on the aigentic service, so it needs aigentic's run right — resolved from the same
// shared identity (the user's Linux groups), so hosuto can gate the tab without asking aigentic.
const AI = 'hp_aigentic_run';

export function Dashboard(props: ServiceContextProps) {
  const { user, nav } = props;
  const t = useT();

  const segs = (nav.path ?? '').split('/').filter(Boolean);
  const serverId = segs[0] ?? '';
  const wanted = (segs[1] ?? 'reach') as Tab;

  const canPlay = userHasRight(user, PLAY);
  const canHost = userHasRight(user, HOST);
  const canAdmin = userHasRight(user, ADMIN);

  if (!canPlay && !canHost && !canAdmin) {
    return (
      <ContentRegion>
        <Panel title={t('hosuto.title')} className="p-4">
          <Text color="secondary">{t('hosuto.noRight')}</Text>
        </Panel>
      </ContentRegion>
    );
  }

  return (
    <ContentRegion>
      {serverId ? (
        <ServerScreen
          key={serverId}
          {...props}
          serverId={serverId}
          wanted={wanted}
          canPlay={canPlay}
          canHost={canHost}
          canAdmin={canAdmin}
        />
      ) : (
        <ServerList {...props} onOpen={(id) => nav.navigate(id)} />
      )}
    </ContentRegion>
  );
}

interface ScreenProps extends ServiceContextProps {
  serverId: string;
  wanted: Tab;
  canPlay: boolean;
  canHost: boolean;
  canAdmin: boolean;
}

function ServerScreen({ serverId, wanted, canPlay, canHost, canAdmin, ...props }: ScreenProps) {
  const { api, nav } = props;
  const t = useT();
  const q = useLiveQuery<ServerView>(() => api.get<ServerView>(`servers/${serverId}`), 10000, [serverId]);
  const srv = q.data;

  useEffect(() => {
    nav.setTitle(srv ? srv.name : null);
    return () => nav.setTitle(null);
  }, [nav, srv?.name]);

  if (!srv) {
    return q.loading ? (
      <Stack direction="row" align="center" gap={2}>
        <Spinner />
        <Text color="secondary">{t('hosuto.loading')}</Text>
      </Stack>
    ) : (
      <EmptyState
        icon={<ServerIcon />}
        title={t('hosuto.notFound')}
        action={
          <Button variant="secondary" iconLeft={<ChevronLeftIcon />} onClick={() => nav.navigate('')}>
            {t('hosuto.back')}
          </Button>
        }
      />
    );
  }

  const canManage = canHost && (srv.owned || canAdmin);
  // Lifecycle is broader than management: an op-level member may start and stop a server they were
  // added to — that is what "op" means to the people using it, and the backend agrees.
  const canControl = srv.owned || canAdmin || srv.level === 'op';

  const canAskAi = userHasRight(props.user, AI);

  const options: SegmentedOption<Tab>[] = [{ value: 'reach', label: t('hosuto.tabReach') }];
  if (canManage) {
    options.push({ value: 'players', label: t('hosuto.tabPlayers') });
    options.push({ value: 'modding', label: t('hosuto.tabModding') });
    options.push({ value: 'files', label: t('hosuto.tabFiles') });
  }
  if (canAskAi) options.push({ value: 'ai', label: t('hosuto.tabAi') });
  if (canPlay) options.push({ value: 'export', label: t('hosuto.tabExport') });

  const tab: Tab = options.some((o) => o.value === wanted) ? wanted : 'reach';
  const state = srv.status?.state ?? 'inactive';

  return (
    <Stack gap={4}>
      <Stack direction="row" align="center" justify="between" wrap gap={3}>
        <Stack direction="row" align="center" gap={2}>
          <Button variant="ghost" size="sm" iconLeft={<ChevronLeftIcon />} onClick={() => nav.navigate('')}>
            {t('hosuto.back')}
          </Button>
          <Heading level={2}>{srv.name}</Heading>
          <Badge variant={state === 'active' ? 'success' : state === 'failed' ? 'danger' : 'neutral'}>
            {t(`hosuto.state.${state}`)}
          </Badge>
          {!srv.owned && <Badge variant="neutral">{t('hosuto.guest')}</Badge>}
        </Stack>
        <SegmentedControl value={tab} onChange={(v) => nav.navigate(`${serverId}/${v}`)} options={options} />
      </Stack>

      {tab === 'reach' && <Reachability {...props} srv={srv} canControl={canControl} onChanged={q.refresh} />}
      {tab === 'players' && canManage && <Players {...props} srv={srv} />}
      {tab === 'modding' && canManage && <Modding {...props} srv={srv} onChanged={q.refresh} />}
      {tab === 'files' && canManage && <Files {...props} srv={srv} />}
      {tab === 'ai' && canAskAi && <AskAi {...props} srv={srv} />}
      {tab === 'export' && canPlay && <ClientExport {...props} srv={srv} />}
    </Stack>
  );
}
