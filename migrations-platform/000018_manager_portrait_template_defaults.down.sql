UPDATE platform_manager.portrait_template
SET topics = '[]'::jsonb, version = version + 1
WHERE singleton AND version = 2 AND topics = $portrait$[
  {"id":"de2d75af-2628-5695-a389-9b34d9946a4b","title":"Звонки","prompts":["Слушает ли кто-то звонки менеджеров, есть ли скрипты?"],"answer_mode":"note","leader":"Сона","warmth_steps":["рассказали","стоит","показали","пользуется"]},
  {"id":"adb1e408-9286-58bd-8e1e-ae1a648ba8fb","title":"Контроль сотрудников и отчёты","prompts":["Как сейчас контролируете менеджеров, какие отчёты нужны?"],"answer_mode":"note","leader":"Наш Контроль активности","market_categories":["Аналитика"],"warmth_steps":["рассказали","стоит","показали","пользуется"]},
  {"id":"69b30f8c-ee36-580d-99df-ced3b013d3d4","title":"Дубли","prompts":["Есть проблемы с дубликатами?"],"answer_mode":"yes_no","leader":"Наш Автопоиск дублей","market_categories":["Поиск дублей"],"warmth_steps":["рассказали","стоит","показали","пользуется"]},
  {"id":"34a98089-18fd-50b3-984b-0db20cc9e449","title":"Телефония","prompts":["Устраивает текущая телефония?"],"answer_mode":"yes_no","leader":"МегаФон","market_categories":["Телефония"],"warmth_steps":["рассказали","стоит","пользуется"]},
  {"id":"8d3548f9-654c-5e15-81e8-d68010285842","title":"Мессенджеры","prompts":["Говорили про Wappi?"],"answer_mode":"yes_no","leader":"Wappi","market_categories":["Мессенджеры"],"warmth_steps":["рассказали","стоит","пользуется"]},
  {"id":"2a22caca-077f-5aa2-9ca9-ef113f957257","title":"Кто занимается настройками","prompts":["Кто у вас настраивает amoCRM?"],"answer_mode":"note"},
  {"id":"1eee1161-2d64-5ca0-97c2-50351cc349a5","title":"Запросы по автоматизации","prompts":["Есть запросы по автоматизации?"],"answer_mode":"yes_no"},
  {"id":"23219098-2b76-5d22-9f1d-05ac72f6fbad","title":"Лицензия amoCRM","prompts":["Продлевать amoCRM будут через нас?"],"answer_mode":"yes_no","warmth_steps":["рассказали","продлили через нас"]}
]$portrait$::jsonb
  AND NOT EXISTS (
    SELECT 1 FROM platform_manager.account_portraits
    WHERE topic_notes ?| ARRAY['de2d75af-2628-5695-a389-9b34d9946a4b', 'adb1e408-9286-58bd-8e1e-ae1a648ba8fb', '69b30f8c-ee36-580d-99df-ced3b013d3d4', '34a98089-18fd-50b3-984b-0db20cc9e449', '8d3548f9-654c-5e15-81e8-d68010285842', '2a22caca-077f-5aa2-9ca9-ef113f957257', '1eee1161-2d64-5ca0-97c2-50351cc349a5', '23219098-2b76-5d22-9f1d-05ac72f6fbad']
  );
