// AskAi is hosuto's "Ask AI" tab: aigentic's chat, bound to ONE server and wired to hosuto's tools.
//
// Two things make it more than a private chat:
//   1. The thread is SHARED and PERSISTENT. It lives in hosuto (GET/POST servers/<id>/chat), so it
//      survives reloads and every operator of the server sees the same history — polled live, and
//      each user turn labelled with who asked. Operators = owner, admin, op-level members; the tab
//      is gated on exactly that (canControl), the same people who may start and stop the server.
//   2. It is agentic. On send it mints a short-lived, server-scoped MCP token and hands it to
//      aigentic's claude-cli engine, so the model can actually operate the server (status, start/
//      stop, whitelist, mods, files, logs) through hosuto's MCP tools.
//
// The model call itself runs in the browser, billed to the sending operator's own aigentic account
// (subscription or API key). hosuto only records the exchange in the shared thread — the split that
// keeps the LLM in aigentic and the server chat in hosuto.
import { useEffect, useRef, useState, type KeyboardEvent } from 'react';
import {
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
  useLiveQuery,
  useT,
  type ServiceContextProps,
} from '@holistic/ui';
import type { ChatMsg, ChatResp, ServerView } from './types';

// The aigentic result envelope (a prizm Response whose Data is the aigentic Result).
interface RunResponse {
  data: { output: string };
}

const SCROLL_ID = 'hosuto-ai-scroll';
const scrollEl = () => document.getElementById(SCROLL_ID);

// clean strips the trailing "Assistant:" the model may echo back from the transcript.
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

export function AskAi({ api, apiFor, ui, user, srv }: Pick<ServiceContextProps, 'api' | 'apiFor' | 'ui' | 'user'> & { srv: ServerView }) {
  const t = useT();
  // The shared thread, polled so operators see each other's turns land.
  const q = useLiveQuery<ChatResp>(() => api.get<ChatResp>(`servers/${srv.id}/chat`), 5000, [srv.id]);
  const history: ChatMsg[] = q.data?.messages ?? [];

  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  // The operator's own turn, shown optimistically until the shared thread reflects it.
  const [pending, setPending] = useState<string | null>(null);
  const tokenRef = useRef<string | null>(null);
  const prevLen = useRef(0);

  // A new message (loaded, sent, or received) jumps to the bottom; the "Working…" row stays in view.
  useEffect(() => {
    const el = scrollEl();
    const total = history.length + (pending ? 1 : 0);
    if (el && (total > prevLen.current || busy)) el.scrollTop = el.scrollHeight;
    prevLen.current = total;
  }, [history.length, pending, busy]);

  async function mcpToken(): Promise<string> {
    if (tokenRef.current) return tokenRef.current;
    const res = await api.post<{ token: string }>('mcp/token', { serverId: srv.id });
    tokenRef.current = res.token;
    return res.token;
  }

  async function send() {
    const text = input.trim();
    if (!text || busy) return;
    setPending(text);
    setInput('');
    setBusy(true);
    try {
      const token = await mcpToken();
      // The whole shared thread plus this turn is sent as one transcript; the model continues after
      // "Assistant:". It sees every operator's turns, which is the point of a shared chat.
      const transcript =
        [...history, { role: 'user', content: text }].map((m) => `${m.role === 'user' ? 'User' : 'Assistant'}: ${m.content}`).join('\n\n') + '\n\nAssistant:';
      const res = await apiFor('aigentic').post<RunResponse>('run', {
        header: { kind: 'claude-cli' },
        data: { prompt: transcript, system: systemPrompt(srv), mcp: [{ name: 'hosuto', token }] },
      });
      const answer = clean(res.data.output);
      // Persist the exchange to the shared thread, then refresh so it (and anyone else's turns) show.
      await api.post(`servers/${srv.id}/chat`, { user: text, assistant: answer });
      setPending(null);
      q.refresh();
    } catch (e) {
      setPending(null);
      const msg = (e as Error).message || '';
      const description = /unavailable|credential|subscription|no Claude/i.test(msg) ? t('hosuto.ai.noEngine') : msg;
      ui.toast({ title: t('hosuto.ai.failed'), description, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  function onKey(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      void send();
    }
  }

  const empty = history.length === 0 && !pending;

  return (
    <Stack gap={3} className="h-full">
      <ScrollArea id={SCROLL_ID} className="grow max-h-[58vh] min-h-[30vh] pr-1">
        {empty ? (
          <EmptyState title={srv.name} description={t('hosuto.ai.empty')} />
        ) : (
          <Stack gap={4}>
            {history.map((m, i) =>
              m.role === 'user' ? (
                <Stack key={i} gap={1} align="end">
                  {m.author && m.author !== user.username && (
                    <Text variant="caption" color="tertiary">
                      {m.author}
                    </Text>
                  )}
                  <Box className="self-end max-w-[85%] rounded-md bg-accent/15 px-3 py-2">
                    <Text className="whitespace-pre-wrap leading-relaxed">{m.content}</Text>
                  </Box>
                </Stack>
              ) : (
                <Stack key={i} gap={1} className="max-w-full">
                  <Badge variant="accent">AI</Badge>
                  {m.content ? <Markdown text={m.content} /> : <Text color="secondary">{t('hosuto.ai.emptyReply')}</Text>}
                </Stack>
              ),
            )}
            {pending && (
              <Box className="self-end max-w-[85%] rounded-md bg-accent/15 px-3 py-2 opacity-70">
                <Text className="whitespace-pre-wrap leading-relaxed">{pending}</Text>
              </Box>
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

      <Stack direction="row" gap={2} align="end">
        <Stack grow>
          <Textarea
            value={input}
            onChange={(e) => setInput(e.target.value)}
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
  );
}
