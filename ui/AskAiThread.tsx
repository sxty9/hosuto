// AskAiThread is one conversation of the "Ask AI" tab, in REAL TIME. It subscribes to a per-server
// Server-Sent Events stream, so new turns and presence ("who is typing / asking the AI") arrive
// instantly — two operators can sit in the same conversation and watch it update live. The model
// call still runs in the browser (billed to the sending operator's own aigentic account); on success
// the exchange is persisted and pushed to everyone by hosuto.
//
// Presence: while composing, the client heartbeats "typing"; while a request is in flight, "working".
// The others see "IchBinsHenry is typing…" / "… is asking the AI…". Heartbeats expire server-side, so
// a vanished client stops showing on its own.
import { useCallback, useEffect, useRef, useState, type KeyboardEvent } from 'react';
import {
  Avatar,
  Badge,
  Box,
  Button,
  EmptyState,
  Markdown,
  ScrollArea,
  Spinner,
  Stack,
  Text,
  Textarea,
  useT,
  type ServiceContextProps,
} from '@holistic/ui';
import { faceUrl } from './face';
import type { ChatMsg, Conversation, PresenceEntry, ServerView } from './types';

interface RunResponse {
  data: { output: string; engine?: string; model?: string };
}

const ENGINE_LABEL: Record<string, string> = {
  'claude-cli': 'Claude CLI',
  'claude-api': 'Claude API',
  ollama: 'Local',
  choose: 'Auto',
};
const engineLabel = (e?: string) => (e ? (ENGINE_LABEL[e] ?? e) : 'AI');

const scrollId = (convId: string) => `hosuto-ai-scroll-${convId}`;

function clean(s: string): string {
  return s.replace(/^\s*Assistant:\s*/i, '').trim();
}

function systemPrompt(srv: ServerView): string {
  return [
    `You are the assistant for the Minecraft server "${srv.name}" (address ${srv.host}, Minecraft ${srv.mcVersion}, loader ${srv.loader}).`,
    `You can inspect and operate THIS server through the hosuto tools: status, players, logs, start/stop/restart, autostart, whitelist, join policy, mods and files.`,
    `This connection is already bound to this server, so you may omit the "server" argument on every tool.`,
    `This is a shared operator chat — more than one person may be talking to you. Prefer calling a tool over guessing. Before anything disruptive — stopping or restarting the server, removing a mod, or removing a member — confirm with the user first.`,
    `Answer concisely, in the user's language.`,
  ].join(' ');
}

type PresenceState = 'idle' | 'typing' | 'working';

export function AskAiThread({
  api,
  apiFor,
  ui,
  user,
  srv,
  convId,
  getToken,
  onPosted,
}: Pick<ServiceContextProps, 'api' | 'apiFor' | 'ui' | 'user'> & {
  srv: ServerView;
  convId: string;
  getToken: () => Promise<string>;
  onPosted: () => void;
}) {
  const t = useT();
  const [messages, setMessages] = useState<ChatMsg[]>([]);
  const [present, setPresent] = useState<PresenceEntry[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  const [pending, setPending] = useState<string | null>(null);
  const prevLen = useRef(0);

  // ── live stream ───────────────────────────────────────────────────────────────────────
  useEffect(() => {
    setLoaded(false);
    setMessages([]);
    setPresent([]);
    const es = new EventSource(api.url(`servers/${srv.id}/chats/${convId}/events`));
    es.addEventListener('conv', (e) => {
      try {
        const c = JSON.parse((e as MessageEvent).data) as Conversation;
        setMessages(c.messages ?? []);
        setLoaded(true);
      } catch {
        /* ignore a malformed frame */
      }
    });
    es.addEventListener('presence', (e) => {
      try {
        const p = JSON.parse((e as MessageEvent).data) as { present: PresenceEntry[] };
        setPresent(p.present ?? []);
      } catch {
        /* ignore */
      }
    });
    return () => es.close();
  }, [api, srv.id, convId]);

  // ── presence heartbeat (this operator's own activity) ─────────────────────────────────
  const stateRef = useRef<PresenceState>('idle');
  const beatRef = useRef<number | null>(null);
  const idleTimerRef = useRef<number | null>(null);

  const postPresence = useCallback(
    (state: PresenceState) => {
      void api.post(`servers/${srv.id}/chats/${convId}/presence`, { state }).catch(() => {});
    },
    [api, srv.id, convId],
  );

  const setMyPresence = useCallback(
    (state: PresenceState) => {
      if (stateRef.current === state) return;
      stateRef.current = state;
      postPresence(state);
      if (beatRef.current) {
        window.clearInterval(beatRef.current);
        beatRef.current = null;
      }
      if (state !== 'idle') {
        beatRef.current = window.setInterval(() => postPresence(state), 3000);
      }
    },
    [postPresence],
  );

  // Reset presence + timers when leaving a conversation.
  useEffect(() => {
    return () => {
      setMyPresence('idle');
      if (beatRef.current) window.clearInterval(beatRef.current);
      if (idleTimerRef.current) window.clearTimeout(idleTimerRef.current);
    };
  }, [convId, setMyPresence]);

  function onInput(v: string) {
    setInput(v);
    if (stateRef.current === 'working') return;
    setMyPresence('typing');
    if (idleTimerRef.current) window.clearTimeout(idleTimerRef.current);
    idleTimerRef.current = window.setTimeout(() => {
      if (stateRef.current === 'typing') setMyPresence('idle');
    }, 4000);
  }

  // ── scroll ────────────────────────────────────────────────────────────────────────────
  useEffect(() => {
    const el = document.getElementById(scrollId(convId));
    const total = messages.length + (pending ? 1 : 0);
    if (el && (total > prevLen.current || busy)) el.scrollTop = el.scrollHeight;
    prevLen.current = total;
  }, [messages.length, pending, busy, convId]);

  async function send() {
    const text = input.trim();
    if (!text || busy) return;
    setPending(text);
    setInput('');
    setBusy(true);
    if (idleTimerRef.current) window.clearTimeout(idleTimerRef.current);
    setMyPresence('working');
    try {
      const token = await getToken();
      const transcript =
        [...messages, { role: 'user', content: text }].map((m) => `${m.role === 'user' ? 'User' : 'Assistant'}: ${m.content}`).join('\n\n') + '\n\nAssistant:';
      const res = await apiFor('aigentic').post<RunResponse>('run', {
        header: { kind: 'claude-cli' },
        data: { prompt: transcript, system: systemPrompt(srv), mcp: [{ name: 'hosuto', token }] },
      });
      const answer = clean(res.data.output);
      const conv = await api.post<Conversation>(`servers/${srv.id}/chats/${convId}`, { user: text, assistant: answer, engine: res.data.engine, model: res.data.model });
      setMessages(conv.messages ?? []); // instant for me; the SSE 'conv' echo confirms it
      setPending(null);
      onPosted();
    } catch (e) {
      setPending(null);
      const msg = (e as Error).message || '';
      const description = /unavailable|credential|subscription|no Claude/i.test(msg) ? t('hosuto.ai.noEngine') : msg;
      ui.toast({ title: t('hosuto.ai.failed'), description, variant: 'error' });
    } finally {
      setBusy(false);
      setMyPresence('idle');
    }
  }

  function onKey(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      void send();
    }
  }

  const others = present.filter((p) => p.author !== user.username);
  const typing = others.filter((p) => p.state === 'typing').map((p) => p.name || p.author);
  const working = others.filter((p) => p.state === 'working').map((p) => p.name || p.author);
  const empty = loaded && messages.length === 0 && !pending;

  return (
    <Stack gap={3} className="h-full">
      <ScrollArea id={scrollId(convId)} className="grow max-h-[54vh] min-h-[30vh] pr-1">
        {!loaded ? (
          <Stack align="center" justify="center" className="min-h-[24vh]">
            <Spinner className="h-5 w-5" />
          </Stack>
        ) : empty ? (
          <EmptyState title={srv.name} description={t('hosuto.ai.empty')} />
        ) : (
          <Stack gap={4}>
            {messages.map((m, i) =>
              m.role === 'user' ? (
                <Stack key={i} gap={1} align="end">
                  {m.author && (
                    <Stack direction="row" align="center" gap={2}>
                      <Text variant="caption" color="tertiary">
                        {m.name || m.author}
                      </Text>
                      <Avatar name={m.name || m.author} src={faceUrl(api, m.author, 40)} size={20} />
                    </Stack>
                  )}
                  <Box className="self-end max-w-[85%] rounded-md bg-accent/15 px-3 py-2">
                    <Text className="whitespace-pre-wrap leading-relaxed">{m.content}</Text>
                  </Box>
                </Stack>
              ) : (
                <Stack key={i} gap={1} className="max-w-full">
                  <Stack direction="row" align="center" gap={2}>
                    <Badge variant="accent">{engineLabel(m.engine)}</Badge>
                    {m.model && (
                      <Text variant="caption" color="tertiary">
                        {m.model}
                      </Text>
                    )}
                  </Stack>
                  {m.content ? <Markdown text={m.content} /> : <Text color="secondary">{t('hosuto.ai.emptyReply')}</Text>}
                </Stack>
              ),
            )}
            {pending && (
              <Stack gap={1} align="end">
                <Stack direction="row" align="center" gap={2}>
                  <Text variant="caption" color="tertiary">
                    {user.username}
                  </Text>
                  <Avatar name={user.username} src={faceUrl(api, user.username, 40)} size={20} />
                </Stack>
                <Box className="self-end max-w-[85%] rounded-md bg-accent/15 px-3 py-2 opacity-70">
                  <Text className="whitespace-pre-wrap leading-relaxed">{pending}</Text>
                </Box>
              </Stack>
            )}
          </Stack>
        )}
        {busy && (
          <Stack direction="row" align="center" gap={2} className="mt-3">
            <Spinner className="h-4 w-4" />
            <Text variant="footnote" color="secondary">
              {t('hosuto.ai.thinking')}
            </Text>
          </Stack>
        )}
      </ScrollArea>

      <Stack gap={1}>
        {(working.length > 0 || typing.length > 0) && (
          <Stack gap={0} className="h-4 justify-center">
            {working.length > 0 ? (
              <Text variant="caption" color="tertiary">
                {t('hosuto.ai.asking', { who: working.join(', ') })}
              </Text>
            ) : (
              <Text variant="caption" color="tertiary">
                {t('hosuto.ai.typing', { who: typing.join(', ') })}
              </Text>
            )}
          </Stack>
        )}
        <Stack direction="row" gap={2} align="end">
          <Stack grow>
            <Textarea
              value={input}
              onChange={(e) => onInput(e.target.value)}
              onBlur={() => {
                if (stateRef.current === 'typing') setMyPresence('idle');
              }}
              onKeyDown={onKey}
              rows={2}
              className="w-full"
              placeholder={t('hosuto.ai.placeholder')}
            />
          </Stack>
          <Button variant="primary" loading={busy} disabled={!input.trim()} onClick={send}>
            Send
          </Button>
        </Stack>
      </Stack>
    </Stack>
  );
}
