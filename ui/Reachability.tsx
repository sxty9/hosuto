// Reachability answers the only two questions a player actually has: what do I type into
// Minecraft, and is anyone there. "Reachable" is not the same as "running": the unit can be up
// while the game is still loading the world, so the status comes from a real Server List Ping,
// not from systemd's opinion.
import { useEffect, useState } from 'react';
import {
  Badge,
  Box,
  Button,
  CopyIcon,
  IconButton,
  Panel,
  Spinner,
  Stack,
  Switch,
  Text,
  cn,
  useLiveQuery,
  useT,
  type ServiceContextProps,
} from '@holistic/ui';
import type { Diagnose, RunState, ServerView, Status } from './types';

const DOT: Record<RunState, string> = {
  active: 'bg-success',
  activating: 'bg-warning',
  inactive: 'bg-fill/40',
  failed: 'bg-danger',
};

type Action = 'start' | 'stop' | 'restart';

export function Reachability({
  api,
  ui,
  srv,
  canControl,
  onChanged,
}: Pick<ServiceContextProps, 'api' | 'ui'> & {
  srv: ServerView;
  canControl: boolean;
  onChanged: () => void;
}) {
  const t = useT();
  const q = useLiveQuery<Status>(() => api.get<Status>(`servers/${srv.id}/status`), 5000, [srv.id]);
  const [busy, setBusy] = useState(false);
  const [autostartBusy, setAutostartBusy] = useState(false);

  const st = q.data;
  const state: RunState = st?.state ?? srv.status?.state ?? 'inactive';
  const running = state === 'active' || state === 'activating';
  const autostart = st?.autostart ?? srv.status?.autostart ?? false;

  // When a start fails, fetch the console log + a short AI explanation ONCE (the effect re-runs only
  // when the state transitions, so a 5s status poll that stays "failed" does not re-ask the AI).
  const [diag, setDiag] = useState<Diagnose | null>(null);
  const [diagLoading, setDiagLoading] = useState(false);
  useEffect(() => {
    if (state !== 'failed' || !canControl) {
      setDiag(null);
      return;
    }
    let live = true;
    setDiagLoading(true);
    setDiag(null);
    api
      .get<Diagnose>(`servers/${srv.id}/diagnose`)
      .then((d) => live && setDiag(d))
      .catch(() => live && setDiag(null))
      .finally(() => live && setDiagLoading(false));
    return () => {
      live = false;
    };
  }, [state, srv.id, canControl, api]);

  async function toggleAutostart(next: boolean) {
    setAutostartBusy(true);
    try {
      await api.put(`servers/${srv.id}/autostart`, { enabled: next });
      ui.toast({ title: next ? t('hosuto.autostartOn') : t('hosuto.autostartOff'), variant: 'success' });
      q.refresh();
    } catch (e) {
      ui.toast({ title: t('hosuto.actionFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setAutostartBusy(false);
    }
  }

  async function act(action: Action) {
    setBusy(true);
    try {
      await api.post(`servers/${srv.id}/${action}`);
      ui.toast({ title: t(`hosuto.did.${action}`), variant: 'success' });
      q.refresh();
      onChanged();
    } catch (e) {
      ui.toast({ title: t('hosuto.actionFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(srv.host);
      ui.toast({ title: t('hosuto.copied'), variant: 'success' });
    } catch {
      ui.toast({ title: t('hosuto.copyFailed'), description: srv.host, variant: 'error' });
    }
  }

  return (
    <Stack gap={4}>
      <Panel title={t('hosuto.address')} className="p-4">
        <Stack direction="row" align="center" gap={2}>
          <Text variant="title3" weight="semibold">
            {srv.host}
          </Text>
          <IconButton label={t('hosuto.copy')} variant="ghost" onClick={copy}>
            <CopyIcon />
          </IconButton>
        </Stack>
      </Panel>

      <Panel title={t('hosuto.status')} className="p-4">
        <Stack gap={3}>
          <Stack direction="row" align="center" gap={3} wrap>
            <Stack direction="row" align="center" gap={2}>
              <Box className={cn('h-2.5 w-2.5 shrink-0 rounded-full', DOT[state])} />
              <Text weight="medium">{t(`hosuto.state.${state}`)}</Text>
            </Stack>
            {q.loading && !st && <Spinner className="h-4 w-4" />}
            {st && st.state === 'active' && !st.reachable && <Badge variant="warning">{t('hosuto.unreachable')}</Badge>}
            {st?.reachable && (
              <Badge variant={st.online > 0 ? 'success' : 'neutral'}>
                {t('hosuto.playersOnline', { online: st.online, max: st.max })}
              </Badge>
            )}
          </Stack>

          {st?.sample && st.sample.length > 0 && (
            <Text variant="footnote" color="secondary">
              {st.sample.slice(0, 8).join(' · ')}
            </Text>
          )}

          {canControl && (
            <Stack direction="row" gap={2}>
              <Button variant="primary" size="sm" disabled={busy || running} onClick={() => void act('start')}>
                {t('hosuto.start')}
              </Button>
              <Button variant="secondary" size="sm" disabled={busy || !running} onClick={() => void act('stop')}>
                {t('hosuto.stop')}
              </Button>
              <Button variant="secondary" size="sm" disabled={busy || !running} onClick={() => void act('restart')}>
                {t('hosuto.restart')}
              </Button>
            </Stack>
          )}
        </Stack>
      </Panel>

      {canControl && state === 'failed' && (
        <Panel title={t('hosuto.startFailed')} className="p-4">
          <Stack gap={3}>
            {diagLoading && (
              <Stack direction="row" align="center" gap={2}>
                <Spinner className="h-4 w-4" />
                <Text variant="footnote" color="secondary">
                  {t('hosuto.diagnosing')}
                </Text>
              </Stack>
            )}
            {diag?.diagnosis && (
              <Box className="rounded-md border border-danger/30 bg-danger/5 p-3">
                <Text variant="footnote" color="secondary" weight="medium">
                  {t('hosuto.aiDiagnosis')}
                  {diag.model ? ` · ${diag.model}` : ''}
                </Text>
                <Text className="mt-1 whitespace-pre-wrap">{diag.diagnosis}</Text>
              </Box>
            )}
            {!diagLoading && !diag?.diagnosis && diag?.diagnosisError === 'no-credential' && (
              <Text variant="footnote" color="secondary">
                {t('hosuto.diagnoseNoCredential')}
              </Text>
            )}
            {diag?.log && (
              <Box>
                <Text variant="footnote" color="secondary" weight="medium">
                  {t('hosuto.consoleLog')}
                </Text>
                <Box className="mt-1 max-h-72 overflow-auto rounded-md border border-line/20 bg-fill/5 p-3">
                  <Text className="whitespace-pre-wrap break-words font-mono text-xs leading-relaxed">{diag.log}</Text>
                </Box>
              </Box>
            )}
          </Stack>
        </Panel>
      )}

      {srv.owned && (
        <Panel title={t('hosuto.autostart')} className="p-4">
          <Switch checked={autostart} disabled={autostartBusy} label={t('hosuto.autostartLabel')} onChange={(v) => void toggleAutostart(v)} />
        </Panel>
      )}
    </Stack>
  );
}
