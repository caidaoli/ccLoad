    // 首页摘要：全局计数与入口协议/认证类型/模型/渠道各维度汇总
    let statsData = { by_client_protocol: {}, by_auth_type: {}, by_model: [], by_channel: [] };
    // statsData 对应的时间范围查询串，排行跳转日志时沿用，保证日志与所见排行同一区间
    let statsDateRangeQuery = '';
    let usageRankMetric = 'requests';
    // Token 构成悬停/点击选中的分段（tone，空为未选中）
    let tokenMixActive = '';
    // 迷你趋势当前序列与悬停的数据点（metric:index，空为未悬停）
    let sparkSeries = null;
    let sparkHover = '';

    // 当前选中的时间范围
    let currentTimeRange = 'today';
    let currentCustomTimeRange = null;
    let serviceHealthModel = null;
    let dashboardLoadGeneration = 0;
    let dashboardUpdatedAt = null;

    const SPARK_MAX_POINTS = 96;
    const USAGE_RANK_LIMIT = 5;

    // 时间范围同步到 URL（range/start_time/end_time）与 localStorage，刷新/分享后保持一致
    const INDEX_FILTER_KEY = 'index.filters';
    const INDEX_RANGE_VALUES = ['today', 'yesterday', 'day_before_yesterday', 'this_week', 'last_week', 'this_month', 'last_month', 'custom'];
    const INDEX_FILTER_FIELDS = [
      {
        key: 'range',
        queryKeys: ['range'],
        defaultValue: 'today',
        includeInQuery(value) {
          return Boolean(value) && value !== 'today';
        }
      },
      {
        key: 'customStartTime',
        queryKeys: ['start_time'],
        defaultValue: '',
        includeInQuery(value, values) {
          return values?.range === 'custom' && Boolean(value);
        }
      },
      {
        key: 'customEndTime',
        queryKeys: ['end_time'],
        defaultValue: '',
        includeInQuery(value, values) {
          return values?.range === 'custom' && Boolean(value);
        }
      }
    ];

    // 入口协议的显示名与图标；Claude Code 是像素画（横向每格 1.5/24），16px 时每格正好 1px，其余 14px；
    // Codex、System One 与对应渠道是同一标志，复用渠道头像（authType）
    const PROTOCOL_USAGE_ROWS = Object.freeze([
      { key: 'anthropic', label: 'Claude Code', iconSize: 16, icon: '<path fill-rule="evenodd" d="M20.998 10.949H24v3.102h-3v3.028h-1.487V20H18v-2.921h-1.487V20H15v-2.921H9V20H7.488v-2.921H6V20H4.487v-2.921H3V14.05H0V10.95h3V5h17.998v5.949zM6 10.949h1.488V8.102H6v2.847zm10.51 0H18V8.102h-1.49v2.847z"/>' },
      { key: 'codex', label: 'Codex', authType: 'codex_oauth' },
      { key: 'openai', label: 'OpenAI', icon: '<path d="M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z"/>' },
      { key: 'gemini', label: 'Google Gemini', icon: '<path d="M20.616 10.835a14.147 14.147 0 01-4.45-3.001 14.111 14.111 0 01-3.678-6.452.503.503 0 00-.975 0 14.134 14.134 0 01-3.679 6.452 14.155 14.155 0 01-4.45 3.001c-.65.28-1.318.505-2.002.678a.502.502 0 000 .975c.684.172 1.35.397 2.002.677a14.147 14.147 0 014.45 3.001 14.112 14.112 0 013.679 6.453.502.502 0 00.975 0c.172-.685.397-1.351.677-2.003a14.145 14.145 0 013.001-4.45 14.113 14.113 0 016.453-3.678.503.503 0 000-.975 13.245 13.245 0 01-2.003-.678z"/>' },
      { key: 'system_one', label: 'System One', authType: 'system_one' }
    ]);

    // 模型名 → 品牌图标：关键词与先后顺序取自 lobehub ModelIcon（features/modelConfig），首个命中生效；
    // 同厂商复用请求协议行（protocol）或渠道类型（authType）的标志与底色，GLM 各代统一用 Z.ai 标志；
    // 其余为 lobehub 单色标志，底色见 .usage-avatar--<key>；Gemma 标志内留白大、MiMo 是文字标，按 lobehub Avatar 放大
    const MODEL_ICON_RULES = Object.freeze([
      { pattern: /gpt-|(^|\/)o[134]|codex|text-embedding-|tts-|whisper-|davinci|babbage|computer-use|openai/, protocol: 'openai' },
      { pattern: /(^|[/-])glm-|chatglm/, authType: 'zai_oauth' },
      { pattern: /claude/, authType: 'anthropic_oauth' },
      { pattern: /llama/, key: 'meta', icon: '<path fill-rule="evenodd" d="M6.897 4c1.915 0 3.516.932 5.43 3.376l.282-.373c.19-.246.383-.484.58-.71l.313-.35C14.588 4.788 15.792 4 17.225 4c1.273 0 2.469.557 3.491 1.516l.218.213c1.73 1.765 2.917 4.71 3.053 8.026l.011.392.002.25c0 1.501-.28 2.759-.818 3.7l-.14.23-.108.153c-.301.42-.664.758-1.086 1.009l-.265.142-.087.04a3.493 3.493 0 01-.302.118 4.117 4.117 0 01-1.33.208c-.524 0-.996-.067-1.438-.215-.614-.204-1.163-.56-1.726-1.116l-.227-.235c-.753-.812-1.534-1.976-2.493-3.586l-1.43-2.41-.544-.895-1.766 3.13-.343.592C7.597 19.156 6.227 20 4.356 20c-1.21 0-2.205-.42-2.936-1.182l-.168-.184c-.484-.573-.837-1.311-1.043-2.189l-.067-.32a8.69 8.69 0 01-.136-1.288L0 14.468c.002-.745.06-1.49.174-2.23l.1-.573c.298-1.53.828-2.958 1.536-4.157l.209-.34c1.177-1.83 2.789-3.053 4.615-3.16L6.897 4zm-.033 2.615l-.201.01c-.83.083-1.606.673-2.252 1.577l-.138.199-.01.018c-.67 1.017-1.185 2.378-1.456 3.845l-.004.022a12.591 12.591 0 00-.207 2.254l.002.188c.004.18.017.36.04.54l.043.291c.092.503.257.908.486 1.208l.117.137c.303.323.698.492 1.17.492 1.1 0 1.796-.676 3.696-3.641l2.175-3.4.454-.701-.139-.198C9.11 7.3 8.084 6.616 6.864 6.616zm10.196-.552l-.176.007c-.635.048-1.223.359-1.82.933l-.196.198c-.439.462-.887 1.064-1.367 1.807l.266.398c.18.274.362.56.55.858l.293.475 1.396 2.335.695 1.114c.583.926 1.03 1.6 1.408 2.082l.213.262c.282.326.529.54.777.673l.102.05c.227.1.457.138.718.138.176.002.35-.023.518-.073.338-.104.61-.32.813-.637l.095-.163.077-.162c.194-.459.29-1.06.29-1.785l-.006-.449c-.08-2.871-.938-5.372-2.2-6.798l-.176-.189c-.67-.683-1.444-1.074-2.27-1.074z"/>' },
      { pattern: /gemini/, protocol: 'gemini' },
      { pattern: /gemma/, key: 'gemma', iconSize: 18, icon: '<path fill-rule="evenodd" d="M12.34 5.953a8.233 8.233 0 01-.247-1.125V3.72a8.25 8.25 0 015.562 2.232H12.34zm-.69 0c.113-.373.199-.755.257-1.145V3.72a8.25 8.25 0 00-5.562 2.232h5.304zm-5.433.187h5.373a7.98 7.98 0 01-.267.696 8.41 8.41 0 01-1.76 2.65L6.216 6.14zm-.264-.187H2.977v.187h2.915a8.436 8.436 0 00-2.357 5.767H0v.186h3.535a8.436 8.436 0 002.357 5.767H2.977v.186h2.976v2.977h.187v-2.915a8.436 8.436 0 005.767 2.357V24h.186v-3.535a8.436 8.436 0 005.767-2.357v2.915h.186v-2.977h2.977v-.186h-2.915a8.436 8.436 0 002.357-5.767H24v-.186h-3.535a8.436 8.436 0 00-2.357-5.767h2.915v-.187h-2.977V2.977h-.186v2.915a8.436 8.436 0 00-5.767-2.357V0h-.186v3.535A8.436 8.436 0 006.14 5.892V2.977h-.187v2.976zm6.14 14.326a8.25 8.25 0 005.562-2.233H12.34c-.108.367-.19.743-.247 1.126v1.107zm-.186-1.087a8.015 8.015 0 00-.258-1.146H6.345a8.25 8.25 0 005.562 2.233v-1.087zm-8.186-7.285h1.107a8.23 8.23 0 001.125-.247V6.345a8.25 8.25 0 00-2.232 5.562zm1.087.186H3.72a8.25 8.25 0 002.232 5.562v-5.304a8.012 8.012 0 00-1.145-.258zm15.47-.186a8.25 8.25 0 00-2.232-5.562v5.315c.367.108.743.19 1.126.247h1.107zm-1.086.186c-.39.058-.772.144-1.146.258v5.304a8.25 8.25 0 002.233-5.562h-1.087zm-1.332 5.69V12.41a7.97 7.97 0 00-.696.267 8.409 8.409 0 00-2.65 1.76l3.346 3.346zm0-6.18v-5.45l-.012-.013h-5.451c.076.235.162.468.26.696a8.698 8.698 0 001.819 2.688 8.698 8.698 0 002.688 1.82c.228.097.46.183.696.259zM6.14 17.848V12.41c.235.078.468.167.696.267a8.403 8.403 0 012.688 1.799 8.404 8.404 0 011.799 2.688c.1.228.19.46.267.696H6.152l-.012-.012zm0-6.245V6.326l3.29 3.29a8.716 8.716 0 01-2.594 1.728 8.14 8.14 0 01-.696.259zm6.257 6.257h5.277l-3.29-3.29a8.716 8.716 0 00-1.728 2.594 8.135 8.135 0 00-.259.696zm-2.347-7.81a9.435 9.435 0 01-2.88 1.96 9.14 9.14 0 012.88 1.94 9.14 9.14 0 011.94 2.88 9.435 9.435 0 011.96-2.88 9.14 9.14 0 012.88-1.94 9.435 9.435 0 01-2.88-1.96 9.434 9.434 0 01-1.96-2.88 9.14 9.14 0 01-1.94 2.88z"/>' },
      { pattern: /kimi|moonshot/, key: 'moonshot', icon: '<path fill-rule="evenodd" d="M1.052 16.916l9.539 2.552a21.007 21.007 0 00.06 2.033l5.956 1.593a11.997 11.997 0 01-5.586.865l-.18-.016-.044-.004-.084-.009-.094-.01a11.605 11.605 0 01-.157-.02l-.107-.014-.11-.016a11.962 11.962 0 01-.32-.051l-.042-.008-.075-.013-.107-.02-.07-.015-.093-.019-.075-.016-.095-.02-.097-.023-.094-.022-.068-.017-.088-.022-.09-.024-.095-.025-.082-.023-.109-.03-.062-.02-.084-.025-.093-.028-.105-.034-.058-.019-.08-.026-.09-.031-.066-.024a6.293 6.293 0 01-.044-.015l-.068-.025-.101-.037-.057-.022-.08-.03-.087-.035-.088-.035-.079-.032-.095-.04-.063-.028-.063-.027a5.655 5.655 0 01-.041-.018l-.066-.03-.103-.047-.052-.024-.096-.046-.062-.03-.084-.04-.086-.044-.093-.047-.052-.027-.103-.055-.057-.03-.058-.032a6.49 6.49 0 01-.046-.026l-.094-.053-.06-.034-.051-.03-.072-.041-.082-.05-.093-.056-.052-.032-.084-.053-.061-.039-.079-.05-.07-.047-.053-.035a7.785 7.785 0 01-.054-.036l-.044-.03-.044-.03a6.066 6.066 0 01-.04-.028l-.057-.04-.076-.054-.069-.05-.074-.054-.056-.042-.076-.057-.076-.059-.086-.067-.045-.035-.064-.052-.074-.06-.089-.073-.046-.039-.046-.039a7.516 7.516 0 01-.043-.037l-.045-.04-.061-.053-.07-.062-.068-.06-.062-.058-.067-.062-.053-.05-.088-.084a13.28 13.28 0 01-.099-.097l-.029-.028-.041-.042-.069-.07-.05-.051-.05-.053a6.457 6.457 0 01-.168-.179l-.08-.088-.062-.07-.071-.08-.042-.049-.053-.062-.058-.068-.046-.056a7.175 7.175 0 01-.027-.033l-.045-.055-.066-.082-.041-.052-.05-.064-.02-.025a11.99 11.99 0 01-1.44-2.402zm-1.02-5.794l11.353 3.037a20.468 20.468 0 00-.469 2.011l10.817 2.894a12.076 12.076 0 01-1.845 2.005L.657 15.923l-.016-.046-.035-.104a11.965 11.965 0 01-.05-.153l-.007-.023a11.896 11.896 0 01-.207-.741l-.03-.126-.018-.08-.021-.097-.018-.081-.018-.09-.017-.084-.018-.094c-.026-.141-.05-.283-.071-.426l-.017-.118-.011-.083-.013-.102a12.01 12.01 0 01-.019-.161l-.005-.047a12.12 12.12 0 01-.034-2.145zm1.593-5.15l11.948 3.196c-.368.605-.705 1.231-1.01 1.875l11.295 3.022c-.142.82-.368 1.612-.668 2.365l-11.55-3.09L.124 10.26l.015-.1.008-.049.01-.067.015-.087.018-.098c.026-.148.056-.295.088-.442l.028-.124.02-.085.024-.097c.022-.09.045-.18.07-.268l.028-.102.023-.083.03-.1.025-.082.03-.096.026-.082.031-.095a11.896 11.896 0 011.01-2.232zm4.442-4.4L17.352 4.59a20.77 20.77 0 00-1.688 1.721l7.823 2.093c.267.852.442 1.744.513 2.665L2.106 5.213l.045-.065.027-.04.04-.055.046-.065.055-.076.054-.072.064-.086.05-.065.057-.073.055-.07.06-.074.055-.069.065-.077.054-.066.066-.077.053-.06.072-.082.053-.06.067-.074.054-.058.073-.078.058-.06.063-.067.168-.17.1-.098.059-.056.076-.071a12.084 12.084 0 012.272-1.677zM12.017 0h.097l.082.001.069.001.054.002.068.002.046.001.076.003.047.002.06.003.054.002.087.005.105.007.144.011.088.007.044.004.077.008.082.008.047.005.102.012.05.006.108.014.081.01.042.006.065.01.207.032.07.012.065.011.14.026.092.018.11.022.046.01.075.016.041.01L14.7.3l.042.01.065.015.049.012.071.017.096.024.112.03.113.03.113.032.05.015.07.02.078.024.073.023.05.016.05.016.076.025.099.033.102.036.048.017.064.023.093.034.11.041.116.045.1.04.047.02.06.024.041.018.063.026.04.018.057.025.11.048.1.046.074.035.075.036.06.028.092.046.091.045.102.052.053.028.049.026.046.024.06.033.041.022.052.029.088.05.106.06.087.051.057.034.053.032.096.059.088.055.098.062.036.024.064.041.084.056.04.027.062.042.062.043.023.017c.054.037.108.075.161.114l.083.06.065.048.056.043.086.065.082.064.04.03.05.041.086.069.079.065.085.071c.712.6 1.353 1.283 1.909 2.031L7.222.994l.062-.027.065-.028.081-.034.086-.035c.113-.045.227-.09.341-.131l.096-.035.093-.033.084-.03.096-.031c.087-.03.176-.058.264-.085l.091-.027.086-.025.102-.03.085-.023.1-.026L9.04.37l.09-.023.091-.022.095-.022.09-.02.098-.021.091-.02.095-.018.092-.018.1-.018.091-.016.098-.017.092-.014.097-.015.092-.013.102-.013.091-.012.105-.012.09-.01.105-.01c.093-.01.186-.018.28-.024l.106-.008.09-.005.11-.006.093-.004.1-.004.097-.002.099-.002.197-.002z"/>' },
      { pattern: /qwen|qwq|qvq|wanx|tongyi/, key: 'qwen', icon: '<path fill-rule="evenodd" d="M12.604 1.34c.393.69.784 1.382 1.174 2.075a.18.18 0 00.157.091h5.552c.174 0 .322.11.446.327l1.454 2.57c.19.337.24.478.024.837-.26.43-.513.864-.76 1.3l-.367.658c-.106.196-.223.28-.04.512l2.652 4.637c.172.301.111.494-.043.77-.437.785-.882 1.564-1.335 2.34-.159.272-.352.375-.68.37-.777-.016-1.552-.01-2.327.016a.099.099 0 00-.081.05 575.097 575.097 0 01-2.705 4.74c-.169.293-.38.363-.725.364-.997.003-2.002.004-3.017.002a.537.537 0 01-.465-.271l-1.335-2.323a.09.09 0 00-.083-.049H4.982c-.285.03-.553-.001-.805-.092l-1.603-2.77a.543.543 0 01-.002-.54l1.207-2.12a.198.198 0 000-.197 550.951 550.951 0 01-1.875-3.272l-.79-1.395c-.16-.31-.173-.496.095-.965.465-.813.927-1.625 1.387-2.436.132-.234.304-.334.584-.335a338.3 338.3 0 012.589-.001.124.124 0 00.107-.063l2.806-4.895a.488.488 0 01.422-.246c.524-.001 1.053 0 1.583-.006L11.704 1c.341-.003.724.032.9.34zm-3.432.403a.06.06 0 00-.052.03L6.254 6.788a.157.157 0 01-.135.078H3.253c-.056 0-.07.025-.041.074l5.81 10.156c.025.042.013.062-.034.063l-2.795.015a.218.218 0 00-.2.116l-1.32 2.31c-.044.078-.021.118.068.118l5.716.008c.046 0 .08.02.104.061l1.403 2.454c.046.081.092.082.139 0l5.006-8.76.783-1.382a.055.055 0 01.096 0l1.424 2.53a.122.122 0 00.107.062l2.763-.02a.04.04 0 00.035-.02.041.041 0 000-.04l-2.9-5.086a.108.108 0 010-.113l.293-.507 1.12-1.977c.024-.041.012-.062-.035-.062H9.2c-.059 0-.073-.026-.043-.077l1.434-2.505a.107.107 0 000-.114L9.225 1.774a.06.06 0 00-.053-.031zm6.29 8.02c.046 0 .058.02.034.06l-.832 1.465-2.613 4.585a.056.056 0 01-.05.029.058.058 0 01-.05-.029L8.498 9.841c-.02-.034-.01-.052.028-.054l.216-.012 6.722-.012z"/>' },
      { pattern: /minimax|abab/, key: 'minimax', icon: '<path fill-rule="evenodd" d="M16.278 2c1.156 0 2.093.927 2.093 2.07v12.501a.74.74 0 00.744.709.74.74 0 00.743-.709V9.099a2.06 2.06 0 012.071-2.049A2.06 2.06 0 0124 9.1v6.561a.649.649 0 01-.652.645.649.649 0 01-.653-.645V9.1a.762.762 0 00-.766-.758.762.762 0 00-.766.758v7.472a2.037 2.037 0 01-2.048 2.026 2.037 2.037 0 01-2.048-2.026v-12.5a.785.785 0 00-.788-.753.785.785 0 00-.789.752l-.001 15.904A2.037 2.037 0 0113.441 22a2.037 2.037 0 01-2.048-2.026V18.04c0-.356.292-.645.652-.645.36 0 .652.289.652.645v1.934c0 .263.142.506.372.638.23.131.514.131.744 0a.734.734 0 00.372-.638V4.07c0-1.143.937-2.07 2.093-2.07zm-5.674 0c1.156 0 2.093.927 2.093 2.07v11.523a.648.648 0 01-.652.645.648.648 0 01-.652-.645V4.07a.785.785 0 00-.789-.78.785.785 0 00-.789.78v14.013a2.06 2.06 0 01-2.07 2.048 2.06 2.06 0 01-2.071-2.048V9.1a.762.762 0 00-.766-.758.762.762 0 00-.766.758v3.8a2.06 2.06 0 01-2.071 2.049A2.06 2.06 0 010 12.9v-1.378c0-.357.292-.646.652-.646.36 0 .653.29.653.646V12.9c0 .418.343.757.766.757s.766-.339.766-.757V9.099a2.06 2.06 0 012.07-2.048 2.06 2.06 0 012.071 2.048v8.984c0 .419.343.758.767.758.423 0 .766-.339.766-.758V4.07c0-1.143.937-2.07 2.093-2.07z"/>' },
      { pattern: /hunyuan|(^|\/)hy\d/, key: 'hunyuan', icon: '<path fill-rule="evenodd" d="M12 0c6.627 0 12 5.373 12 12s-5.373 12-12 12S0 18.627 0 12 5.373 0 12 0zm1.652 1.123l-.01-.001c.533.097 1.023.233 1.41.404 6.084 2.683 7.396 9.214 1.601 14.338a3.781 3.781 0 01-5.337-.328 3.654 3.654 0 01-.884-3.044c-1.934.6-3.295 2.305-3.524 4.45-.204 1.912.324 4.044 2.056 5.634l.245.067C10.1 22.876 11.036 23 12 23c6.075 0 11-4.925 11-11 0-5.513-4.056-10.08-9.348-10.877zM2.748 6.21c-.178.269-.348.536-.51.803l-.235.394.078-.167A10.957 10.957 0 001 12c0 4.919 3.228 9.083 7.682 10.49l.214.065C3.523 18.528 2.84 14.149 6.47 8.68A2.234 2.234 0 102.748 6.21zm10.157-5.172c4.408 1.33 3.61 5.41 2.447 6.924-.86 1.117-2.922 1.46-3.708 2.238-.666.657-1.077 1.462-1.212 2.291A5.303 5.303 0 0112 12.258a5.672 5.672 0 001.404-11.169 10.51 10.51 0 00-.5-.052z"/>' },
      { pattern: /(^|\/)grok-/, key: 'grok', icon: '<path fill-rule="evenodd" d="M9.27 15.29l7.978-5.897c.391-.29.95-.177 1.137.272.98 2.369.542 5.215-1.41 7.169-1.951 1.954-4.667 2.382-7.149 1.406l-2.711 1.257c3.889 2.661 8.611 2.003 11.562-.953 2.341-2.344 3.066-5.539 2.388-8.42l.006.007c-.983-4.232.242-5.924 2.75-9.383.06-.082.12-.164.179-.248l-3.301 3.305v-.01L9.267 15.292M7.623 16.723c-2.792-2.67-2.31-6.801.071-9.184 1.761-1.763 4.647-2.483 7.166-1.425l2.705-1.25a7.808 7.808 0 00-1.829-1A8.975 8.975 0 005.984 5.83c-2.533 2.536-3.33 6.436-1.962 9.764 1.022 2.487-.653 4.246-2.34 6.022-.599.63-1.199 1.259-1.682 1.925l7.62-6.815"/>' },
      { pattern: /deepseek/, key: 'deepseek', icon: '<path fill-rule="evenodd" d="M23.748 4.482c-.254-.124-.364.113-.512.234-.051.039-.094.09-.137.136-.372.397-.806.657-1.373.626-.829-.046-1.537.214-2.163.848-.133-.782-.575-1.248-1.247-1.548-.352-.156-.708-.311-.955-.65-.172-.241-.219-.51-.305-.774-.055-.16-.11-.323-.293-.35-.2-.031-.278.136-.356.276-.313.572-.434 1.202-.422 1.84.027 1.436.633 2.58 1.838 3.393.137.093.172.187.129.323-.082.28-.18.552-.266.833-.055.179-.137.217-.329.14a5.526 5.526 0 01-1.736-1.18c-.857-.828-1.631-1.742-2.597-2.458a11.365 11.365 0 00-.689-.471c-.985-.957.13-1.743.388-1.836.27-.098.093-.432-.779-.428-.872.004-1.67.295-2.687.684a3.055 3.055 0 01-.465.137 9.597 9.597 0 00-2.883-.102c-1.885.21-3.39 1.102-4.497 2.623C.082 8.606-.231 10.684.152 12.85c.403 2.284 1.569 4.175 3.36 5.653 1.858 1.533 3.997 2.284 6.438 2.14 1.482-.085 3.133-.284 4.994-1.86.47.234.962.327 1.78.397.63.059 1.236-.03 1.705-.128.735-.156.684-.837.419-.961-2.155-1.004-1.682-.595-2.113-.926 1.096-1.296 2.746-2.642 3.392-7.003.05-.347.007-.565 0-.845-.004-.17.035-.237.23-.256a4.173 4.173 0 001.545-.475c1.396-.763 1.96-2.015 2.093-3.517.02-.23-.004-.467-.247-.588zM11.581 18c-2.089-1.642-3.102-2.183-3.52-2.16-.392.024-.321.471-.235.763.09.288.207.486.371.739.114.167.192.416-.113.603-.673.416-1.842-.14-1.897-.167-1.361-.802-2.5-1.86-3.301-3.307-.774-1.393-1.224-2.887-1.298-4.482-.02-.386.093-.522.477-.592a4.696 4.696 0 011.529-.039c2.132.312 3.946 1.265 5.468 2.774.868.86 1.525 1.887 2.202 2.891.72 1.066 1.494 2.082 2.48 2.914.348.292.625.514.891.677-.802.09-2.14.11-3.054-.614zm1-6.44a.306.306 0 01.415-.287.302.302 0 01.2.288.306.306 0 01-.31.307.303.303 0 01-.304-.308zm3.11 1.596c-.2.081-.399.151-.59.16a1.245 1.245 0 01-.798-.254c-.274-.23-.47-.358-.552-.758a1.73 1.73 0 01.016-.588c.07-.327-.008-.537-.239-.727-.187-.156-.426-.199-.688-.199a.559.559 0 01-.254-.078c-.11-.054-.2-.19-.114-.358.028-.054.16-.186.192-.21.356-.202.767-.136 1.146.016.352.144.618.408 1.001.782.391.451.462.576.685.914.176.265.336.537.445.848.067.195-.019.354-.25.452z"/>' },
      { pattern: /(^|\/)mimo-/, key: 'mimo', iconSize: 22, icon: '<path fill-rule="evenodd" d="M.958 15.936a.459.459 0 01.459.44v2.729a.46.46 0 01-.918 0v-2.729a.459.459 0 01.459-.44zm4.814-2.035a.46.46 0 01.553.45v4.754a.458.458 0 11-.918 0V15.48L3.74 17.202a.462.462 0 01-.655.016.462.462 0 01-.065-.082L.628 14.67a.459.459 0 01.658-.637l2.124 2.187 2.127-2.188a.46.46 0 01.235-.13zm2.068.004a.46.46 0 01.458.445v4.755a.46.46 0 01-.458.458.459.459 0 01-.458-.458V14.35a.459.459 0 01.458-.445zm1.973 2.014a.46.46 0 01.46.457v2.729a.46.46 0 01-.784.324.46.46 0 01-.134-.324v-2.729a.46.46 0 01.458-.458zm.002-2.045a.458.458 0 01.328.157l2.127 2.19 2.125-2.19a.459.459 0 01.784.318v4.756a.46.46 0 01-.455.458.46.46 0 01-.458-.458V15.48l-1.667 1.723a.46.46 0 01-.65.008l-.005-.005c0-.002-.002-.002-.004-.003l-2.455-2.534a.46.46 0 01-.008-.667.461.461 0 01.338-.128zm6.797 1.206a.46.46 0 01.53.651A1.966 1.966 0 0019.81 18.4a.462.462 0 01.623.18.46.46 0 01-.181.624 2.863 2.863 0 01-1.38.353l-.142-.004a2.88 2.88 0 01-2.393-4.263.461.461 0 01.274-.21zm.864-.931a2.884 2.884 0 013.915 3.914.46.46 0 01-.402.24l-.057-.004a.458.458 0 01-.164-.055.46.46 0 01-.182-.622 1.967 1.967 0 00-2.669-2.67.459.459 0 11-.441-.803zM9.59 6.368c1.481 0 1.696 1.202 1.696 1.654v2.648h-.917v-.432c-.26.346-.792.535-1.36.535-.133 0-1.289-.03-1.384-1.136-.082-.932.675-1.61 2.053-1.61h.691c0-.563-.367-.886-.983-.886-.44.013-.864.174-1.2.458l-.36-.664c.484-.379 1.012-.567 1.764-.567zm4.427.1c1.263 0 2.082.97 2.083 2.15 0 1.181-.824 2.154-2.083 2.154-1.26 0-2.084-.972-2.084-2.152 0-1.18.82-2.153 2.084-2.153zm6.801.015c.68 0 1.202.465 1.197 1.548v2.642H21.1V8.29c0-.312-.002-.98-.63-.98s-.628.667-.628.838v2.524h-.89V8.148c0-.17-.001-.838-.63-.838-.628 0-.628.668-.628.98v2.383h-.917v-4.03h.917V7a1.22 1.22 0 01.947-.516c.398 0 .76.193.982.686a1.321 1.321 0 011.195-.686zm-18.093.872l1.457-1.772H5.32L3.311 8.07l2.14 2.602H4.24L2.725 8.796 1.21 10.672H0L2.138 8.07.13 5.583h1.138l1.458 1.772zm4.149 3.317h-.916V6.644h.916v4.028zm16.99 0h-.916V6.644h.916v4.028zM9.925 8.71c-1.055 0-1.359.412-1.326.742.032.329.324.537.757.537a1.013 1.013 0 001.014-.968l.002-.31h-.447zM14.018 7.3c-.663 0-1.184.487-1.184 1.32 0 .832.52 1.32 1.184 1.32.662 0 1.182-.49 1.182-1.32 0-.832-.52-1.32-1.182-1.32zM6.417 5.001a.568.568 0 01.587.582.588.588 0 01-1.175 0A.57.57 0 016.417 5zm16.991 0a.57.57 0 01.592.582.588.588 0 01-1.174 0 .57.57 0 01.357-.542.572.572 0 01.225-.04z"/>' },
      { pattern: /(^|\/)jev(-|$)/, authType: 'system_one' }
    ]);

    // 渠道认证类型的显示名与渠道管理页筛选一致；图标共用 channel-provider-icons.js
    const AUTH_TYPE_LABEL_KEYS = Object.freeze({
      api_key: 'channels.authTypeAPI',
      codex_oauth: 'channels.authTypeCodex',
      antigravity_oauth: 'channels.authTypeAntigravity',
      codebuddy_oauth: 'channels.authTypeCodeBuddy',
      xai_oauth: 'channels.authTypeXAI',
      anthropic_oauth: 'channels.authTypeAnthropic',
      zai_oauth: 'channels.authTypeZAI',
      system_one: 'channels.authTypeSystemOne',
      cursor_oauth: 'channels.authTypeCursor',
      zed_oauth: 'channels.authTypeZed',
      vertex_oauth: 'channels.authTypeVertex'
    });

    // Token 构成的分段顺序即横条绘制顺序
    const TOKEN_MIX_PARTS = Object.freeze([
      { key: 'inputTokens', labelKey: 'index.tokenMix.input', fallback: '输入', tone: 'input' },
      { key: 'outputTokens', labelKey: 'index.tokenMix.output', fallback: '输出', tone: 'output' },
      { key: 'cacheReadTokens', labelKey: 'index.tokenMix.cacheRead', fallback: '缓存读取', tone: 'cache-read' },
      { key: 'cacheCreationTokens', labelKey: 'index.tokenMix.cacheCreation', fallback: '缓存创建', tone: 'cache-write' }
    ]);

    function overviewText(key, fallback, params) {
      if (typeof window.i18nText === 'function') return window.i18nText(key, fallback, params);
      const translated = typeof window.t === 'function' ? window.t(key, params) : key;
      return translated === key ? fallback : translated;
    }

    function toNumber(value) {
      const n = Number(value);
      return Number.isFinite(n) ? n : 0;
    }

    // effective_cost 缺省时视为未打折，等于 total_cost
    function effectiveCostOf(stat) {
      return stat && stat.effective_cost !== undefined && stat.effective_cost !== null
        ? toNumber(stat.effective_cost)
        : toNumber(stat && stat.total_cost);
    }

    // 全局指标：请求计数取响应顶层；费用与 Tokens 按入口协议加总（每个请求只属于一个入口协议）。
    // 入库的 input_tokens 已扣除缓存读写，四类 Token 互不重叠，相加即总量。
    function buildOverviewTotals(summary) {
      const totals = {
        requests: toNumber(summary && summary.total_requests),
        success: toNumber(summary && summary.success_requests),
        error: toNumber(summary && summary.error_requests),
        rate: null,
        cost: 0,
        effectiveCost: 0,
        inputTokens: 0,
        outputTokens: 0,
        cacheReadTokens: 0,
        cacheCreationTokens: 0,
        promptTokens: 0,
        totalTokens: 0,
        cacheRate: null
      };
      for (const stat of Object.values((summary && summary.by_client_protocol) || {})) {
        totals.cost += toNumber(stat && stat.total_cost);
        totals.effectiveCost += effectiveCostOf(stat);
        totals.inputTokens += toNumber(stat && stat.total_input_tokens);
        totals.outputTokens += toNumber(stat && stat.total_output_tokens);
        totals.cacheReadTokens += toNumber(stat && stat.total_cache_read_tokens);
        totals.cacheCreationTokens += toNumber(stat && stat.total_cache_creation_tokens);
      }
      totals.promptTokens = totals.inputTokens + totals.cacheReadTokens + totals.cacheCreationTokens;
      totals.totalTokens = totals.promptTokens + totals.outputTokens;
      if (totals.requests > 0) totals.rate = totals.success / totals.requests;
      if (totals.promptTokens > 0) totals.cacheRate = totals.cacheReadTokens / totals.promptTokens;
      return totals;
    }

    // 费用 KPI 不需要三位小数，满 $1 取两位
    function formatOverviewCost(cost) {
      return formatCost(cost, cost >= 1 ? 2 : undefined);
    }

    // 指标分桶 → 迷你趋势序列；桶数过多时合并相邻桶，保持折线可读。
    // starts/ends 为每个点覆盖的时间段（毫秒），桶缺少时间戳时为 null。
    function buildSparkSeries(metrics, bucketMinutes) {
      const points = Array.isArray(metrics) ? metrics : [];
      const chunk = Math.max(1, Math.ceil(points.length / SPARK_MAX_POINTS));
      const bucketMs = toNumber(bucketMinutes) * 60 * 1000;
      const series = { requests: [], errors: [], cost: [], starts: [], ends: [] };
      for (let start = 0; start < points.length; start += chunk) {
        const group = points.slice(start, start + chunk);
        let requests = 0;
        let errors = 0;
        let cost = 0;
        for (const point of group) {
          if (!point) continue;
          requests += toNumber(point.success) + toNumber(point.error);
          errors += toNumber(point.error);
          cost += effectiveCostOf(point);
        }
        const startMs = Date.parse(group[0] && group[0].ts);
        series.requests.push(requests);
        series.errors.push(errors);
        series.cost.push(cost);
        series.starts.push(Number.isFinite(startMs) ? startMs : null);
        series.ends.push(Number.isFinite(startMs) && bucketMs > 0 ? startMs + group.length * bucketMs : null);
      }
      return series;
    }

    // 序列各点在 viewBox(100×32) 中的坐标，按最大值归一；全零或单点时贴底
    function sparkCoords(values) {
      const max = Math.max(0, ...values);
      const flat = values.length < 2 || max <= 0;
      return values.map((value, index) => ({
        x: values.length < 2 ? 50 : index * 100 / (values.length - 1),
        y: flat ? 31 : 31 - value / max * 28
      }));
    }

    // 单调三次曲线（同 d3.curveMonotoneX）：经过每个数据点，段内不越过两端值，峰值不被抬高、零值不下穿
    function sparkPath(points) {
      const slopes = points.slice(1).map((point, index) => (point.y - points[index].y) / (point.x - points[index].x));
      const tangents = points.map((point, index) => {
        if (index === 0) return slopes[0];
        if (index === points.length - 1) return slopes[index - 1];
        const prev = slopes[index - 1];
        const next = slopes[index];
        if (prev * next <= 0) return 0;
        const h0 = point.x - points[index - 1].x;
        const h1 = points[index + 1].x - point.x;
        const weighted = (prev * h1 + next * h0) / (h0 + h1);
        return Math.sign(prev) * Math.min(2 * Math.abs(prev), 2 * Math.abs(next), Math.abs(weighted));
      });
      const coord = (x, y) => `${x.toFixed(2)} ${y.toFixed(2)}`;
      let path = `M${coord(points[0].x, points[0].y)}`;
      for (let index = 1; index < points.length; index++) {
        const from = points[index - 1];
        const to = points[index];
        const third = (to.x - from.x) / 3;
        path += `C${coord(from.x + third, from.y + tangents[index - 1] * third)} ${coord(to.x - third, to.y - tangents[index] * third)} ${coord(to.x, to.y)}`;
      }
      return path;
    }

    function renderSparkline(id, values) {
      const svg = document.getElementById(id);
      if (!svg) return;
      if (!values) {
        svg.innerHTML = '';
        return;
      }
      if (values.length < 2 || Math.max(0, ...values) <= 0) {
        svg.innerHTML = '<path class="overview-spark-line overview-spark-line--flat" d="M0 31H100"/>';
        return;
      }
      const line = sparkPath(sparkCoords(values));
      svg.innerHTML = `<path class="overview-spark-area" d="${line}L100 32L0 32Z"/><path class="overview-spark-line" d="${line}"/>`;
    }

    function renderSparklines(series) {
      sparkSeries = series;
      hideSparkTooltip();
      renderSparkline('overview-requests-spark', series && series.requests);
      renderSparkline('overview-cost-spark', series && series.cost);
    }

    function hideSparkTooltip() {
      sparkHover = '';
      const tooltip = document.getElementById('overview-spark-tooltip');
      const dot = document.getElementById('overview-spark-dot');
      if (tooltip) tooltip.hidden = true;
      if (dot) dot.hidden = true;
    }

    // 悬停或点击（触屏）迷你趋势：圆点标出最近的数据点，提示框显示其时间段与数值
    function showSparkTooltip(svg, clientX) {
      const metric = svg.dataset.metric;
      const values = sparkSeries && sparkSeries[metric];
      const container = svg.closest('.overview-kpis');
      const tooltip = document.getElementById('overview-spark-tooltip');
      const dot = document.getElementById('overview-spark-dot');
      const timeElement = document.getElementById('overview-spark-tooltip-time');
      const valueElement = document.getElementById('overview-spark-tooltip-value');
      if (!values || values.length === 0 || !container || !tooltip || !dot || !timeElement || !valueElement) {
        hideSparkTooltip();
        return;
      }
      const rect = svg.getBoundingClientRect();
      const ratio = rect.width > 0 ? (clientX - rect.left) / rect.width : 0;
      const index = Math.min(Math.max(Math.round(ratio * (values.length - 1)), 0), values.length - 1);
      const hoverKey = `${metric}:${index}`;
      if (hoverKey === sparkHover) return;
      sparkHover = hoverKey;

      const errors = metric === 'requests' ? sparkSeries.errors : null;
      const point = sparkCoords(values)[index];
      const x = rect.left + point.x / 100 * rect.width;
      const y = rect.top + point.y / 32 * rect.height;
      const containerRect = container.getBoundingClientRect();
      dot.hidden = false;
      dot.style.left = `${x - containerRect.left}px`;
      dot.style.top = `${y - containerRect.top}px`;
      const failedCount = errors ? errors[index] : 0;
      dot.style.setProperty('--spark-color', getComputedStyle(svg).getPropertyValue('--spark-color'));

      const startMs = sparkSeries.starts[index];
      const endMs = sparkSeries.ends[index];
      const hasTime = startMs !== null && endMs !== null;
      const formatter = serviceHealthTimeFormatter();
      timeElement.hidden = !hasTime;
      timeElement.textContent = hasTime
        ? `${formatter.format(new Date(startMs))} – ${formatter.format(new Date(endMs))}`
        : '';
      const value = values[index];
      if (metric === 'cost') valueElement.textContent = formatCost(value);
      else if (failedCount > 0) {
        const failed = overviewText('index.overview.failedCount', `${failedCount}\u00a0失败`, { count: formatNumber(failedCount) });
        const rate = failedCount / value;
        valueElement.textContent = `${formatNumber(value)} · ${failed} (${rate < 0.001 ? '<0.1%' : formatPercent(rate)})`;
      } else valueElement.textContent = formatNumber(value);
      tooltip.hidden = false;
      placeOverviewTooltip(tooltip, container, { left: x, width: 0, top: y - 4, bottom: y + 4 });
    }

    function bindSparklines() {
      for (const svg of document.querySelectorAll('.overview-spark[data-metric]')) {
        const show = event => showSparkTooltip(svg, event.clientX);
        svg.addEventListener('mousemove', show);
        svg.addEventListener('click', show);
        svg.addEventListener('mouseleave', hideSparkTooltip);
      }
    }

    function renderOverviewKpis(totals) {
      document.getElementById('overview-requests').textContent = formatNumber(totals.requests);
      document.getElementById('overview-counts').textContent = overviewText(
        'index.overview.failedCount',
        `${totals.error}\u00a0失败`,
        { count: formatNumber(totals.error) }
      );
      const rateElement = document.getElementById('overview-rate');
      rateElement.textContent = formatPercent(totals.rate);
      // 成功率、缓存率与其他页面共用 ui.js 的四档分级
      rateElement.dataset.state = window.getSuccessRateLevel(totals.rate) || 'unknown';
      document.getElementById('overview-tokens').textContent = formatNumber(totals.totalTokens);
      const cacheRateHtml = `<span style="color: ${window.getCacheHitRateColor(totals.cacheRate)};">${formatPercent(totals.cacheRate)}</span>`;
      document.getElementById('overview-tokens-detail').innerHTML = overviewText(
        'index.overview.cacheRateDetail',
        `缓存率\u00a0${cacheRateHtml}`,
        { rate: cacheRateHtml }
      );
      // 主值为倍率后的实付费用；有倍率时副行给出原价
      const cost = getCostDisplayInfo(totals.cost, totals.effectiveCost);
      document.getElementById('overview-cost').textContent = formatOverviewCost(cost.effectiveCost);
      document.getElementById('overview-cost-detail').textContent = cost.hasMultiplier
        ? overviewText(
          'index.overview.standardCost',
          `原价\u00a0${formatOverviewCost(cost.standardCost)}`,
          { cost: formatOverviewCost(cost.standardCost) }
        )
        : '';
    }

    function tokenMixShare(totals, value) {
      return totals.totalTokens > 0 ? value / totals.totalTokens : 0;
    }

    // 图例每行列出名称、用量与占比
    function renderTokenMix(totals) {
      const bar = document.getElementById('token-mix-bar');
      const legend = document.getElementById('token-mix-legend');
      if (!bar || !legend) return;
      const segments = [];
      legend.innerHTML = TOKEN_MIX_PARTS.map(part => {
        const value = totals[part.key];
        const share = tokenMixShare(totals, value);
        if (share > 0) {
          segments.push(`<span class="token-mix-segment token-mix-segment--${part.tone}" data-part="${part.tone}" style="flex:${share.toFixed(4)} 1 0;"></span>`);
        }
        return `<li class="token-mix-item" data-part="${part.tone}">
          <span class="token-mix-dot token-mix-dot--${part.tone}" aria-hidden="true"></span>
          <span class="token-mix-label">${escapeHtml(overviewText(part.labelKey, part.fallback))}</span>
          <span class="token-mix-value">${formatNumber(value)}</span>
          <span class="token-mix-share">${formatPercent(share)}</span>
        </li>`;
      }).join('');
      bar.innerHTML = segments.join('');
      renderTokenMixActive();
    }

    // 选中分段时横条与图例同时高亮该分段，其余淡化
    function renderTokenMixActive() {
      const container = document.getElementById('token-mix');
      if (!container) return;
      container.toggleAttribute('data-active', Boolean(tokenMixActive));
      for (const element of container.querySelectorAll('[data-part]')) {
        element.classList.toggle('is-active', element.dataset.part === tokenMixActive);
      }
    }

    // 悬停或点击（触屏）色块/图例行选中分段，移出横条与图例取消选中
    function bindTokenMix() {
      const container = document.getElementById('token-mix');
      if (!container) return;
      const selectPart = event => {
        const target = event.target.closest('[data-part]');
        const next = target ? target.dataset.part : '';
        if (next === tokenMixActive) return;
        tokenMixActive = next;
        renderTokenMixActive();
      };
      container.addEventListener('mouseover', selectPart);
      container.addEventListener('click', selectPart);
      container.addEventListener('mouseleave', () => {
        tokenMixActive = '';
        renderTokenMixActive();
      });
    }

    function brandIconHtml(row) {
      const size = row.iconSize || 14;
      return `<span class="usage-avatar usage-avatar--${row.key}" aria-hidden="true"><svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="currentColor">${row.icon}</svg></span>`;
    }

    // 无品牌标志时显示名称首字母，统一用中性底色
    function letterIconHtml(label) {
      return `<span class="usage-avatar usage-avatar--letter" aria-hidden="true">${escapeHtml((label.trim().charAt(0) || '?').toUpperCase())}</span>`;
    }

    function providerAvatarHtml(authType, content) {
      return `<span class="usage-avatar channel-provider-avatar" data-auth-type="${escapeHtml(authType)}" aria-hidden="true">${content}</span>`;
    }

    function authTypeIconHtml(authType, label) {
      const icon = typeof window.channelProviderIconSVG === 'function' ? window.channelProviderIconSVG(authType) : '';
      return icon ? providerAvatarHtml(authType, icon) : letterIconHtml(label);
    }

    // 渠道排行与渠道管理页同一头像（API Key 渠道为首字母）；已删除的渠道没有认证类型，用中性首字母
    function channelIconHtml(row) {
      if (!row.authType || typeof window.channelAvatarContentHTML !== 'function') return letterIconHtml(row.label);
      return providerAvatarHtml(row.authType, window.channelAvatarContentHTML(row.authType, row.label));
    }

    function protocolRowIconHtml(row) {
      return row.authType ? authTypeIconHtml(row.authType, row.label) : brandIconHtml(row);
    }

    function protocolIconHtml(key) {
      return protocolRowIconHtml(PROTOCOL_USAGE_ROWS.find(row => row.key === key));
    }

    function modelIconHtml(row) {
      const model = row.label.toLowerCase();
      const rule = MODEL_ICON_RULES.find(item => item.pattern.test(model));
      if (!rule) return letterIconHtml(row.label);
      if (rule.authType) return authTypeIconHtml(rule.authType, row.label);
      return rule.protocol ? protocolIconHtml(rule.protocol) : brandIconHtml(rule);
    }

    function summaryUsageRow(row, label, stat) {
      return {
        ...row,
        label,
        requests: toNumber(stat && stat.total_requests),
        tokens: toNumber(stat && stat.total_input_tokens) + toNumber(stat && stat.total_output_tokens)
          + toNumber(stat && stat.total_cache_read_tokens) + toNumber(stat && stat.total_cache_creation_tokens),
        cost: effectiveCostOf(stat)
      };
    }

    // 摘要的模型/渠道汇总 → 排行行；已删除的渠道没有名称，以 #ID 显示。
    // 隐藏渠道的 API 令牌会话不返回 by_channel，渠道排行为空。
    // logsFilter 是跳转日志页的精确筛选；已删除的渠道无名称可筛，不提供跳转。
    function buildSummaryUsageRows(summary) {
      const models = Array.isArray(summary && summary.by_model) ? summary.by_model : [];
      const channels = Array.isArray(summary && summary.by_channel) ? summary.by_channel : [];
      return {
        model: models.map(stat => summaryUsageRow(
          { key: String(stat.model), logsFilter: { model: String(stat.model) } },
          String(stat.model),
          stat
        )),
        channel: channels.map(stat => summaryUsageRow(
          {
            key: String(stat.channel_id),
            authType: stat.auth_type || '',
            logsFilter: stat.channel_name ? { channel_name: stat.channel_name } : null
          },
          stat.channel_name || `#${stat.channel_id}`,
          stat
        ))
      };
    }

    // 日志页链接：时间范围参数在前，筛选参数（model / channel_name / client_protocol）在后
    function buildUsageLogsHref(dateRangeQuery, logsFilter) {
      if (!logsFilter) return '';
      const params = new URLSearchParams(dateRangeQuery || '');
      for (const [key, value] of Object.entries(logsFilter)) params.set(key, value);
      return `/web/logs.html?${params.toString()}`;
    }

    // 按指标取前 N 名；占比以该维度全部行之和为分母
    function rankUsageRows(rows, metric, limit = USAGE_RANK_LIMIT) {
      const total = rows.reduce((sum, row) => sum + row[metric], 0);
      return rows
        .filter(row => row[metric] > 0)
        .sort((left, right) => right[metric] - left[metric])
        .slice(0, limit)
        .map(row => ({ ...row, share: row[metric] / total }));
    }

    function formatUsageMetric(value) {
      return usageRankMetric === 'cost' ? formatCost(value) : formatNumber(value);
    }

    function renderUsageRank(listId, rows, iconOf) {
      const list = document.getElementById(listId);
      if (!list) return;
      const ranked = rankUsageRows(rows, usageRankMetric);
      if (ranked.length === 0) {
        list.innerHTML = `<li class="usage-rank-empty">${escapeHtml(overviewText('index.rank.empty', '暂无数据'))}</li>`;
        return;
      }
      // 行底色按占比填充；可筛选的行整行链接到日志页
      list.innerHTML = ranked.map((row, index) => {
        const style = `--usage-share:${(row.share * 100).toFixed(1)}%;`;
        const cells = `<span class="usage-rank-index">${index + 1}</span>
          <span class="usage-rank-name" title="${escapeHtml(row.label)}">${iconOf ? iconOf(row) : ''}<span class="usage-rank-label">${escapeHtml(row.label)}</span></span>
          <span class="usage-rank-value">${escapeHtml(formatUsageMetric(row[usageRankMetric]))}</span>
          <span class="usage-rank-share">${formatPercent(row.share)}</span>`;
        const href = buildUsageLogsHref(statsDateRangeQuery, row.logsFilter);
        return href
          ? `<li><a class="usage-rank-item usage-rank-link" href="${escapeHtml(href)}" style="${style}">${cells}</a></li>`
          : `<li class="usage-rank-item" style="${style}">${cells}</li>`;
      }).join('');
    }

    function renderUsageRanks() {
      const protocolStats = statsData.by_client_protocol || {};
      const usageRows = buildSummaryUsageRows(statsData);
      renderUsageRank('usage-rank-model', usageRows.model, modelIconHtml);
      renderUsageRank('usage-rank-channel', usageRows.channel, channelIconHtml);
      renderUsageRank('usage-rank-protocol',
        PROTOCOL_USAGE_ROWS.map(row => summaryUsageRow(
          { ...row, logsFilter: { client_protocol: row.key } }, row.label, protocolStats[row.key]
        )),
        protocolRowIconHtml);
      renderUsageRank('usage-rank-auth-type',
        Object.entries(statsData.by_auth_type || {}).map(([authType, stat]) => {
          const labelKey = AUTH_TYPE_LABEL_KEYS[authType];
          return summaryUsageRow({ key: authType }, labelKey ? overviewText(labelKey, authType) : authType, stat);
        }),
        row => authTypeIconHtml(row.key, row.label));
    }

    function bindUsageRankMetric() {
      const group = document.getElementById('usage-rank-metric');
      if (!group) return;
      group.addEventListener('click', event => {
        const button = event.target.closest('button[data-metric]');
        if (!button || button.dataset.metric === usageRankMetric) return;
        usageRankMetric = button.dataset.metric;
        for (const item of group.querySelectorAll('button[data-metric]')) {
          const active = item === button;
          item.classList.toggle('active', active);
          item.setAttribute('aria-pressed', String(active));
        }
        renderUsageRanks();
      });
    }

    function buildCurrentDateRangeQuery() {
      return typeof window.buildDateRangeQuery === 'function'
        ? window.buildDateRangeQuery(currentTimeRange, currentCustomTimeRange)
        : `range=${encodeURIComponent(currentTimeRange)}`;
    }

    function currentRangeHours() {
      if (currentTimeRange === 'custom' && currentCustomTimeRange) {
        const startMs = Number(currentCustomTimeRange.startMs);
        const endMs = Number(currentCustomTimeRange.endMs);
        if (Number.isFinite(startMs) && Number.isFinite(endMs) && endMs > startMs) {
          return Math.max((endMs - startMs) / 3600000, 1 / 60);
        }
      }
      return typeof window.getRangeHours === 'function'
        ? window.getRangeHours(currentTimeRange)
        : 24;
    }

    function serviceHealthLocale() {
      return window.i18n && typeof window.i18n.getLocale === 'function' && window.i18n.getLocale() === 'en'
        ? 'en-US'
        : 'zh-CN';
    }

    function serviceHealthTimeFormatter() {
      return new Intl.DateTimeFormat(serviceHealthLocale(), {
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        hourCycle: 'h23'
      });
    }

    function serviceHealthPeriodText() {
      return typeof window.getRangeLabel === 'function'
        ? window.getRangeLabel(currentTimeRange)
        : currentTimeRange;
    }

    function serviceHealthRateLimitedText(count) {
      return overviewText('index.health.rateLimited', `其中限流\u00a0${count}`, { count: formatNumber(count) });
    }

    function hideServiceHealthTooltip() {
      const tooltip = document.getElementById('service-health-tooltip');
      if (tooltip) tooltip.hidden = true;
    }

    function showServiceHealthTooltip(cell, point, formatter, bucketMs) {
      const plot = cell.closest('.service-health-plot');
      const tooltip = document.getElementById('service-health-tooltip');
      const timeElement = document.getElementById('service-health-tooltip-time');
      const successElement = document.getElementById('service-health-tooltip-success');
      const errorElement = document.getElementById('service-health-tooltip-error');
      const rateElement = document.getElementById('service-health-tooltip-rate');
      if (!plot || !tooltip || !timeElement || !successElement || !errorElement || !rateElement) return;

      const intervalMs = bucketMs || 15 * 60 * 1000;
      timeElement.textContent = `${formatter.format(new Date(point.ts))} – ${formatter.format(new Date(point.ts + intervalMs))}`;
      const pendingElement = document.getElementById('service-health-tooltip-pending');
      const statsElement = document.getElementById('service-health-tooltip-stats');
      if (pendingElement && statsElement) {
        pendingElement.hidden = !point.pending;
        pendingElement.textContent = point.pending ? overviewText('index.health.pending', '未到') : '';
        statsElement.hidden = Boolean(point.pending);
      }
      successElement.textContent = formatNumber(point.success);
      errorElement.textContent = formatNumber(point.error);
      rateElement.textContent = `(${formatPercent(point.rate)})`;
      const rateLimitedElement = document.getElementById('service-health-tooltip-rate-limited');
      if (rateLimitedElement) {
        rateLimitedElement.hidden = !(point.rateLimited > 0);
        rateLimitedElement.textContent = point.rateLimited > 0 ? serviceHealthRateLimitedText(point.rateLimited) : '';
      }

      tooltip.hidden = false;
      placeOverviewTooltip(tooltip, plot, cell.getBoundingClientRect());
    }

    // 提示框放在锚点（视口坐标）上方，空间不足时放下方；水平居中并限制在定位容器内
    function placeOverviewTooltip(tooltip, container, anchor) {
      tooltip.dataset.placement = 'top';
      const containerRect = container.getBoundingClientRect();
      const tooltipRect = tooltip.getBoundingClientRect();
      const anchorCenter = anchor.left - containerRect.left + anchor.width / 2;
      const inset = 8;
      const maxLeft = Math.max(inset, containerRect.width - tooltipRect.width - inset);
      const left = Math.min(Math.max(anchorCenter - tooltipRect.width / 2, inset), maxLeft);
      // 上方空间按视口计算，扣除固定顶栏
      const topbar = document.querySelector('.topbar');
      const roomAbove = anchor.top - (topbar ? topbar.getBoundingClientRect().bottom : 0);
      let top = anchor.top - containerRect.top - tooltipRect.height - 12;

      if (roomAbove < tooltipRect.height + 16) {
        top = anchor.bottom - containerRect.top + 12;
        tooltip.dataset.placement = 'bottom';
      }

      tooltip.style.left = `${left}px`;
      tooltip.style.top = `${top}px`;
      const arrowX = Math.min(Math.max(anchorCenter - left, 12), tooltipRect.width - 12);
      tooltip.style.setProperty('--service-health-tooltip-arrow-x', `${arrowX}px`);
    }

    function renderServiceHealth(model) {
      const grid = document.getElementById('service-health-grid');
      const message = document.getElementById('service-health-message');
      if (!grid || !message || !model) return;

      hideServiceHealthTooltip();
      const timeFormatter = serviceHealthTimeFormatter();
      const fragment = document.createDocumentFragment();
      for (const [index, point] of model.points.entries()) {
        const cell = document.createElement('span');
        cell.className = `service-health-cell ${point.state}${point.pending ? ' pending' : ''}`;
        cell.setAttribute('aria-hidden', 'true');
        cell.dataset.index = String(index);
        fragment.appendChild(cell);
      }
      grid.replaceChildren(fragment);
      grid.onmouseover = event => {
        const cell = event.target.closest('.service-health-cell');
        if (!cell || !grid.contains(cell)) return;
        showServiceHealthTooltip(cell, model.points[Number(cell.dataset.index)], timeFormatter, model.bucketMs);
      };
      grid.onmouseleave = hideServiceHealthTooltip;

      const hasData = model.rate !== null;
      const rate = formatPercent(model.rate);
      const period = serviceHealthPeriodText();
      grid.setAttribute('aria-label', hasData
        ? overviewText(
          'index.health.summary',
          `${period}服务成功率 ${rate}，成功 ${model.success} 次，失败 ${model.error} 次`,
          {
            period,
            rate,
            success: formatNumber(model.success),
            error: formatNumber(model.error)
          }
        )
        : overviewText('index.health.noData', `${period}暂无请求数据`, { period }));
      // 请求卡在失败次数后补限流次数（取自分桶数据）
      const rateDetail = document.getElementById('overview-rate-detail');
      if (rateDetail) {
        rateDetail.textContent = model.rateLimited > 0
          ? ` · ${overviewText('index.overview.rateLimited', `限流\u00a0${model.rateLimited}`, { count: formatNumber(model.rateLimited) })}`
          : '';
      }
      message.hidden = true;
      message.textContent = '';
    }

    function renderServiceHealthUnavailable() {
      const message = document.getElementById('service-health-message');
      const rateDetail = document.getElementById('overview-rate-detail');
      if (rateDetail) rateDetail.textContent = '';
      if (message) {
        message.hidden = false;
        message.textContent = overviewText(
          'index.health.unavailable',
          '健康数据暂时无法加载，将在下次刷新时重试。'
        );
        const retryButton = document.createElement('button');
        retryButton.type = 'button';
        retryButton.className = 'btn btn-secondary btn-sm load-retry-btn';
        retryButton.textContent = overviewText('common.retry', '重试');
        retryButton.addEventListener('click', loadDashboard);
        message.appendChild(retryButton);
      }
    }

    async function loadDashboard() {
      const generation = ++dashboardLoadGeneration;
      const dateRangeQuery = buildCurrentDateRangeQuery();
      const grid = document.getElementById('service-health-grid');
      if (grid) grid.setAttribute('aria-busy', 'true');

      const healthRequest = window.ServiceHealth
        ? window.ServiceHealth.buildRequest(dateRangeQuery, currentRangeHours())
        : null;
      const [summaryResult, healthResult] = await Promise.allSettled([
        fetchDataWithAuth(`/dashboard/summary?${dateRangeQuery}`),
        healthRequest
          ? fetchDataWithAuth(`/dashboard/metrics?${healthRequest.query}`)
          : Promise.reject(new Error('ServiceHealth unavailable'))
      ]);

      if (generation !== dashboardLoadGeneration) return;
      dashboardUpdatedAt = new Date();

      if (summaryResult.status === 'fulfilled') {
        if (summaryResult.value) {
          statsData = summaryResult.value;
          statsDateRangeQuery = dateRangeQuery;
        }
      } else {
        console.error('Failed to load stats:', summaryResult.reason);
        showError(overviewText('index.overview.loadFailed', '无法加载统计数据'));
      }
      updateStatsDisplay();

      if (healthResult.status === 'fulfilled') {
        renderSparklines(buildSparkSeries(healthResult.value, healthRequest.bucketMinutes));
        serviceHealthModel = window.ServiceHealth.buildModel(
          healthResult.value,
          healthRequest.bucketMinutes,
          window.ServiceHealth.rangeEndMs(currentTimeRange, currentCustomTimeRange)
        );
        renderServiceHealth(serviceHealthModel);
      } else {
        console.error('Failed to load service health:', healthResult.reason);
        renderSparklines(null);
        renderServiceHealthUnavailable();
      }

      renderUpdatedAt();
      if (grid) grid.setAttribute('aria-busy', 'false');
    }

    // 页头显示最近一次加载完成的时刻，自动刷新时可确认数据新鲜度
    function renderUpdatedAt() {
      const element = document.getElementById('overview-updated');
      if (!element) return;
      const time = new Intl.DateTimeFormat(serviceHealthLocale(), {
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        hourCycle: 'h23'
      }).format(dashboardUpdatedAt);
      element.textContent = overviewText('index.overview.updatedAt', `${time} 更新`, { time });
    }

    // 更新统计显示
    function updateStatsDisplay() {
      const totals = buildOverviewTotals(statsData);
      renderOverviewKpis(totals);
      renderTokenMix(totals);
      renderUsageRanks();
    }

    // 通知系统统一由 ui.js 提供（showSuccess/showError/showNotification）

    // 注销功能（已由 ui.js 的 onLogout 统一处理）

    // 自动刷新由 createAutoRefresh 统一管理（system_settings.auto_refresh_interval_seconds）

    function getIndexFilters() {
      const hasCustomRange = currentTimeRange === 'custom' && currentCustomTimeRange;
      return {
        range: currentTimeRange,
        customStartTime: hasCustomRange ? String(currentCustomTimeRange.startMs) : '',
        customEndTime: hasCustomRange ? String(currentCustomTimeRange.endMs) : ''
      };
    }

    // 解析 URL/本地保存的时间范围；非法值或残缺的自定义区间回落到"本日"
    function resolveIndexTimeRange(values) {
      const range = INDEX_RANGE_VALUES.includes(values?.range) ? values.range : 'today';
      if (range !== 'custom') return { range, customRange: null };
      const startMs = Number(values.customStartTime);
      const endMs = Number(values.customEndTime);
      if (!Number.isFinite(startMs) || !Number.isFinite(endMs) || endMs <= startMs) {
        return { range: 'today', customRange: null };
      }
      return { range, customRange: { startMs: Math.trunc(startMs), endMs: Math.trunc(endMs) } };
    }

    function restoreIndexTimeRange() {
      if (!window.FilterState) return;
      const restored = resolveIndexTimeRange(window.FilterState.restore({
        search: location.search,
        savedFilters: window.FilterState.load(INDEX_FILTER_KEY),
        fields: INDEX_FILTER_FIELDS
      }));
      currentTimeRange = restored.range;
      currentCustomTimeRange = restored.customRange;
    }

    function persistIndexTimeRange() {
      window.persistFilterState({
        key: INDEX_FILTER_KEY,
        values: getIndexFilters(),
        pathname: location.pathname,
        fields: INDEX_FILTER_FIELDS,
        historyMethod: 'replaceState'
      });
    }

    if (typeof module !== 'undefined' && module.exports) {
      module.exports = { buildOverviewTotals, buildSparkSeries, buildSummaryUsageRows, buildUsageLogsHref, rankUsageRows, resolveIndexTimeRange };
    }

    // 页面初始化
    if (typeof window !== 'undefined') window.initPageBootstrap({
      topbarKey: 'index',
      run: () => {
      restoreIndexTimeRange();
      window.bindTimeRangeSelector({
        containerId: 'index-time-range',
        values: INDEX_RANGE_VALUES,
        initialValue: currentTimeRange,
        customRange: currentCustomTimeRange,
        onChange: (range, customRange) => {
          currentTimeRange = range;
          if (range === 'custom') currentCustomTimeRange = customRange;
          persistIndexTimeRange();
          loadDashboard();
        }
      });

      bindUsageRankMetric();
      bindTokenMix();
      bindSparklines();

      // 摘要（含排行）与服务健康检测共用同一日期范围快照。
      loadDashboard();

      if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
        window.i18n.onLocaleChange(() => {
          updateStatsDisplay();
          if (serviceHealthModel) renderServiceHealth(serviceHealthModel);
          if (dashboardUpdatedAt) renderUpdatedAt();
        });
      }

      // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用）
      if (typeof window.createAutoRefresh === 'function') {
        window.createAutoRefresh({ load: loadDashboard }).init();
      }
      }
    });
