package tools

// The browse pack (cont.): find. A plain-language element search: candidates
// are scored against the query on role/name/text/attrs and each match carries a
// usable CSS selector so it can be fed straight to pointer/input.

import (
	"context"
	"encoding/json"
	"strconv"

	"mcptransport"
)

// findExpr is a JS function (q, limit) -> [{score, role, name, selector}].
const findExpr = `(function(q,limit){
var terms=q.toLowerCase().split(/\s+/).filter(Boolean);
function norm(s){return (s||'').toLowerCase();}
function textOf(el){return (el.getAttribute('aria-label')||el.getAttribute('placeholder')||el.getAttribute('title')||el.textContent||'').trim();}
function esc(s){return (window.CSS&&CSS.escape)?CSS.escape(s):s.replace(/[^a-zA-Z0-9_-]/g,'\\$&');}
function selOf(el){
if(el.id)return '#'+esc(el.id);
if(el.name){var s=el.tagName.toLowerCase()+'[name="'+el.name+'"]';if(document.querySelectorAll(s).length===1)return s;}
var path=[],n=el;
while(n&&n.nodeType===1&&n.tagName!=='HTML'){
var tag=n.tagName.toLowerCase(),par=n.parentNode;
var same=[].filter.call(par.children,function(c){return c.tagName===n.tagName;});
if(same.length>1)tag+=':'+(same.indexOf(n)+1);
path.unshift(tag);n=par;
}
return path.join('>');
}
var all=document.querySelectorAll('a,button,input,select,textarea,[role],[onclick],[tabindex],label,summary,h1,h2,h3,h4,li,td,th,[aria-label],[title]');
var out=[],seen={};
for(var i=0;i<all.length;i++){
var el=all[i];
var role=el.getAttribute('role')||el.tagName.toLowerCase();
var name=textOf(el);
if(!name&&!el.getAttribute('role'))continue;
var hay=norm(name+' '+role+' '+el.tagName+' '+(el.getAttribute('placeholder')||'')+' '+(el.getAttribute('title')||'')+' '+(el.getAttribute('aria-label')||''));
var score=0;
for(var k=0;k<terms.length;k++){var t=terms[k];
if(hay.indexOf(t)>=0)score+=2;
if(norm(name)===t)score+=3;
if(norm(role)===t||el.tagName.toLowerCase()===t)score+=1;
}
if(score<=0)continue;
var s=selOf(el);var key=role+'\u0000'+s;
if(seen[key])continue;seen[key]=1;
out.push({score:score,role:role,name:name,selector:s});
}
out.sort(function(x,y){return y.score-x.score;});
return out.slice(0,limit);
})`

var findTool = &mcptransport.Tool{
	Name:        "find",
	Description: "Find elements by describing them: a short plain-language query (\"login button\", \"search bar\", \"delete icon\") ranked against likely interactive/labeled elements. Returns matches with a usable CSS selector, role, name and score.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"query": {Type: mcptransport.TypeString, Description: "a few words describing the element: its purpose, visible text, or hint text"},
			"limit": {Type: mcptransport.TypeNumber, Description: "maximum matches returned (default 10)"},
		},
		Required: []string{"query"},
	},
}

func (c *Core) find(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Query string  `json:"query"`
		Limit float64 `json:"limit"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	if a.Query == "" {
		return mcptransport.ResultError("find needs query"), nil
	}
	limit := int(a.Limit)
	if limit <= 0 {
		limit = 10
	}
	page := c.page()
	val, err := evalValue(ctx, page, findExpr+"("+jsString(a.Query)+","+strconv.Itoa(limit)+")")
	if err != nil {
		return mcptransport.ResultError(err.Error()), nil
	}
	return mcptransport.ResultText(c.cap(string(normalizeJSON(val)), 0)), nil
}
