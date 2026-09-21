(function(){
 'use strict';
 const t=k=>window.dorangI18n?window.dorangI18n.t(k):k;
 const el=(tag,text)=>{const e=document.createElement(tag);if(text!==undefined)e.textContent=text;return e;};
 document.addEventListener('DOMContentLoaded',()=>{
  const root=document.querySelector('[data-setup]');if(!root)return;
  root.querySelectorAll('[data-provider-kind]').forEach(select=>{const update=()=>{if(select.options){const option=select.options[select.selectedIndex];select.form.elements.base_url.placeholder=option?.dataset.endpoint||'https://gateway.example/v1';}select.form.querySelectorAll('[data-kind-parameter]').forEach(field=>{field.hidden=!field.dataset.kindParameter.split(' ').includes(select.value);field.querySelector('input').disabled=field.hidden;});};select.addEventListener('change',update);update();});
  root.querySelectorAll('[data-auth-form]').forEach(form=>{
   const update=()=>{const source=form.elements.source.value,oauth=form.elements.auth.value==='oauth';
    form.querySelector('[data-secret-field]').hidden=source!=='secret';form.querySelector('[data-reference-field]').hidden=!['env','file'].includes(source);form.querySelector('[data-oauth-field]').hidden=!oauth||source==='keep';
    const key=form.querySelector('[data-key-value]'),store=form.querySelector('[data-oauth-value]');key.hidden=oauth;store.hidden=!oauth;key.disabled=oauth||source!=='secret';store.disabled=!oauth||source!=='secret';key.required=!key.disabled;store.required=!store.disabled;
    form.elements.reference.disabled=!['env','file'].includes(source);form.elements.reference.required=!form.elements.reference.disabled;
   };form.elements.source.addEventListener('change',update);form.elements.auth.addEventListener('change',()=>{if(form.elements.source.value==='keep')form.elements.source.value='secret';update();});update();
  });
  root.querySelectorAll('[data-model-form]').forEach(form=>{const update=()=>{let selected=false;for(const option of form.elements.credential.options){option.hidden=option.dataset.provider!==form.elements.provider.value;option.disabled=option.hidden;if(option.selected&&!option.disabled)selected=true;}if(!selected){const first=[...form.elements.credential.options].find(o=>!o.disabled);form.elements.credential.value=first?first.value:'';}};form.elements.provider.addEventListener('change',update);update();});
  let probeResult=null;
  const resultBox=root.querySelector('#discovery-result');
  const render=()=>{if(!probeResult)return;resultBox.hidden=false;resultBox.replaceChildren();const{data,account,provider}=probeResult;
   const messages={ready:'Connection verified. Select a model to add its service binding.',manual:'Automatic model discovery is unavailable for this provider. Enter the upstream model ID manually.',authentication_failed:'Authentication was refused. Update this account’s key or token and try again.',unreachable:'The provider could not be reached. Check the endpoint and network connection.',unavailable:'The model-list endpoint is unavailable. Review its HTTP status or add a model manually.',invalid_response:'The endpoint did not return a supported model list.',failed:'The connection check failed. Reload this page and try again.'};
   resultBox.append(el('h3',account),el('p',t(messages[data.status]||messages.failed)));if(data.http_status)resultBox.append(el('small','HTTP '+data.http_status));
   const list=el('div');list.className='discovered-models';for(const model of data.models||[]){const a=el('a',model);a.className='tag';a.href=root.dataset.base+'/setup?'+new URLSearchParams({provider,credential:account,upstream:model})+'#service-models';list.append(a);}resultBox.append(list);
  };
  root.querySelectorAll('[data-discover]').forEach(button=>button.addEventListener('click',async()=>{
   button.disabled=true;resultBox.hidden=false;resultBox.textContent=t('Checking provider authentication and model access…');
   try{const response=await fetch(root.dataset.base+'/keys/action',{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:new URLSearchParams({csrf:root.dataset.csrf,action:'setup_discover',credential:button.dataset.discover})});const data=response.ok&&response.headers.get('content-type')?.includes('application/json')?await response.json():{status:'failed'};probeResult={data,account:button.dataset.discover,provider:button.dataset.provider};render();}catch(_){probeResult={data:{status:'failed'},account:button.dataset.discover,provider:button.dataset.provider};render();}finally{button.disabled=false;}
  }));
  document.addEventListener('dorang:language',render);
  window.addEventListener('pageshow',event=>{if(event.persisted)root.querySelectorAll('[name=secret]').forEach(input=>{input.value='';});});
 });
})();
