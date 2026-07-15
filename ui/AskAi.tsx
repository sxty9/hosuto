// AskAi is hosuto's "Ask AI" tab: aigentic's chat, bound to a server and wired to hosuto's tools.
//
// Like aigentic's chat it manages MANY conversations — a sidebar to create, pick, search and delete
// them — but the conversations are SHARED and PERSISTENT: they live in hosuto (per server), so every
// operator of the server (owner, admin, op-level members) sees the same list and threads, and they
// survive reloads. The tab is gated on exactly those operators (canControl in Dashboard), the same
// people who may start and stop the server. The MCP token minted for the agentic runs is shared
// across a session's conversations (one per operator, scoped to this server).
import { useEffect, useRef, useState } from 'react';
import {
  Button,
  EmptyState,
  IconButton,
  PlusIcon,
  ScrollArea,
  SearchField,
  ServerIcon,
  Spinner,
  Stack,
  Text,
  TrashIcon,
  useLiveQuery,
  useT,
  type ServiceContextProps,
} from '@holistic/ui';
import { AskAiThread } from './AskAiThread';
import type { ChatsResp, ConvSummary, ServerView } from './types';

export function AskAi(props: Pick<ServiceContextProps, 'api' | 'apiFor' | 'ui' | 'user'> & { srv: ServerView }) {
  const { api, ui, srv } = props;
  const t = useT();
  const q = useLiveQuery<ChatsResp>(() => api.get<ChatsResp>(`servers/${srv.id}/chats`), 5000, [srv.id]);
  const conversations: ConvSummary[] = q.data?.conversations ?? [];

  const [activeId, setActiveId] = useState<string | null>(null);
  const [search, setSearch] = useState('');
  // One MCP token per operator per session, scoped to this server, reused across conversations.
  const tokenRef = useRef<string | null>(null);

  // Default the selection to the newest conversation, and heal it if the active one is deleted.
  useEffect(() => {
    if (conversations.length === 0) {
      if (activeId !== null) setActiveId(null);
      return;
    }
    if (!activeId || !conversations.some((c) => c.id === activeId)) {
      setActiveId(conversations[0].id);
    }
  }, [conversations, activeId]);

  async function getToken(): Promise<string> {
    if (tokenRef.current) return tokenRef.current;
    const res = await api.post<{ token: string }>('mcp/token', { serverId: srv.id });
    tokenRef.current = res.token;
    return res.token;
  }

  async function newChat() {
    try {
      const c = await api.post<{ id: string }>(`servers/${srv.id}/chats`, {});
      setActiveId(c.id);
      q.refresh();
    } catch (e) {
      ui.toast({ title: t('hosuto.ai.failed'), description: (e as Error).message, variant: 'error' });
    }
  }

  async function deleteChat(id: string) {
    const ok = await ui.confirm({ title: t('hosuto.ai.deleteChatTitle'), danger: true, confirmLabel: t('hosuto.remove') });
    if (!ok) return;
    try {
      await api.del(`servers/${srv.id}/chats/${id}`);
      if (id === activeId) setActiveId(null);
      q.refresh();
    } catch (e) {
      ui.toast({ title: t('hosuto.ai.failed'), description: (e as Error).message, variant: 'error' });
    }
  }

  const filtered = search ? conversations.filter((c) => (c.title || '').toLowerCase().includes(search.toLowerCase())) : conversations;

  if (q.loading && !q.data) {
    return (
      <Stack align="center" justify="center" className="min-h-[40vh]">
        <Spinner className="h-6 w-6" />
      </Stack>
    );
  }

  return (
    <Stack direction="row" gap={4} align="stretch" className="min-h-[60vh]">
      <Stack gap={2} className="w-60 shrink-0">
        <Button variant="primary" size="sm" iconLeft={<PlusIcon className="h-4 w-4" />} onClick={newChat}>
          {t('hosuto.ai.newChat')}
        </Button>
        <SearchField value={search} onChange={setSearch} placeholder={t('hosuto.ai.searchChats')} />
        <ScrollArea className="grow max-h-[52vh] -mr-1 pr-1">
          {filtered.length === 0 ? (
            <Text variant="caption" color="tertiary">
              {search ? t('hosuto.ai.noMatch') : t('hosuto.ai.noChats')}
            </Text>
          ) : (
            <Stack gap={1}>
              {filtered.map((c) => (
                <Stack key={c.id} direction="row" align="center" gap={1}>
                  <Button
                    variant={c.id === activeId ? 'secondary' : 'ghost'}
                    size="sm"
                    className="grow min-w-0 justify-start"
                    onClick={() => setActiveId(c.id)}
                  >
                    <Text truncate className="w-full text-left">
                      {c.title || t('hosuto.ai.newChat')}
                    </Text>
                  </Button>
                  <IconButton label={t('hosuto.ai.deleteChat')} size="sm" variant="ghost" onClick={() => void deleteChat(c.id)}>
                    <TrashIcon className="h-4 w-4" />
                  </IconButton>
                </Stack>
              ))}
            </Stack>
          )}
        </ScrollArea>
      </Stack>

      <Stack grow className="min-w-0">
        {activeId ? (
          <AskAiThread key={activeId} {...props} convId={activeId} getToken={getToken} onPosted={q.refresh} />
        ) : (
          <EmptyState icon={<ServerIcon />} title={srv.name} description={t('hosuto.ai.pickOrNew')} action={<Button variant="primary" iconLeft={<PlusIcon className="h-4 w-4" />} onClick={newChat}>{t('hosuto.ai.newChat')}</Button>} />
        )}
      </Stack>
    </Stack>
  );
}
