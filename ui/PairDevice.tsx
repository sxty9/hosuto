// Pairing hands the desktop app its first credential.
//
// The app has no session and must not ask for a password, so the exchange runs the other way: here, in
// the browser where the user is already signed in, the daemon mints a short code; the user carries it
// the few metres to the app, which trades it for a token. The code is single-use and lives minutes.
//
// This surface earns its one line of instruction — the exception the Minimalism maxim allows. A code
// with nothing said about it is a puzzle: the whole object is meaningless until you know it goes in the
// app, and no arrangement of pixels conveys that.
import { useCallback, useEffect, useState } from 'react';
import {
  Box,
  Button,
  CopyIcon,
  Modal,
  Spinner,
  Stack,
  Text,
  useT,
  type ServiceContextProps,
} from '@holistic/ui';
import type { PairStart } from './types';

// The code is read off one screen and typed into another, so it is shown the way it is meant to be
// read: two runs of four. The daemon accepts it back with or without the dash.
function grouped(code: string): string {
  return code.length === 8 ? `${code.slice(0, 4)}-${code.slice(4)}` : code;
}

function mmss(seconds: number): string {
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  return `${m}:${String(s).padStart(2, '0')}`;
}

export function PairDeviceModal({
  api,
  ui,
  onClose,
}: Pick<ServiceContextProps, 'api' | 'ui'> & { onClose: () => void }) {
  const t = useT();
  const [pair, setPair] = useState<PairStart | null>(null);
  const [failed, setFailed] = useState(false);
  const [left, setLeft] = useState(0);

  const mint = useCallback(async () => {
    setFailed(false);
    setPair(null);
    try {
      setPair(await api.post<PairStart>('pair/start'));
    } catch {
      setFailed(true);
    }
  }, [api]);

  useEffect(() => {
    void mint();
  }, [mint]);

  // Tick the countdown against the daemon's absolute expiry rather than a local duration: a laptop that
  // slept through the window must not come back showing four minutes left on a dead code.
  useEffect(() => {
    if (!pair) return;
    const tick = () => setLeft(Math.max(0, pair.expires - Math.floor(Date.now() / 1000)));
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [pair]);

  const expired = !!pair && left === 0;
  // One string carries both halves of what the app must know — where, and who. It parses the same shape.
  const full = pair ? `${pair.host}/${pair.code}` : '';

  async function copy() {
    try {
      await navigator.clipboard.writeText(full);
      ui.toast({ title: t('hosuto.copied'), variant: 'success' });
    } catch {
      ui.toast({ title: t('hosuto.copyFailed'), description: full, variant: 'error' });
    }
  }

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={t('hosuto.pairTitle')}
      size="sm"
      footer={
        <Stack direction="row" justify="end" gap={2}>
          <Button variant="ghost" onClick={onClose}>
            {t('hosuto.close')}
          </Button>
          {expired && (
            <Button variant="primary" onClick={() => void mint()}>
              {t('hosuto.pairNewCode')}
            </Button>
          )}
        </Stack>
      }
    >
      <Stack gap={3} align="center">
        {!pair && !failed && <Spinner />}
        {failed && <Text color="secondary">{t('hosuto.pairFailed')}</Text>}
        {pair && (
          <>
            <Box className="rounded-md border border-line/20 bg-fill/5 px-4 py-3">
              <Text
                className={
                  expired
                    ? 'select-all font-mono text-2xl tracking-widest line-through opacity-40'
                    : 'select-all font-mono text-2xl tracking-widest'
                }
              >
                {grouped(pair.code)}
              </Text>
            </Box>
            <Text variant="footnote" color="secondary">
              {expired ? t('hosuto.pairExpired') : t('hosuto.pairHint', { host: pair.host, left: mmss(left) })}
            </Text>
            {!expired && (
              <Button variant="secondary" size="sm" iconLeft={<CopyIcon />} onClick={copy}>
                {t('hosuto.pairCopy')}
              </Button>
            )}
          </>
        )}
      </Stack>
    </Modal>
  );
}
